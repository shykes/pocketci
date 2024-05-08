package pocketci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dagger.io/dagger"

	"github.com/google/go-github/v61/github"
)

var (
	ErrPushEventIgnored = errors.New("push event ignored due to duplication")
)

const DaggerVersion = "0.11.3"

type Agent struct {
	dag *dagger.Client
}

func NewAgent(dag *dagger.Client) *Agent {
	return &Agent{
		dag: dag,
	}
}

func (agent *Agent) CreateGithubSecret(username, password string) *dagger.Secret {
	contents := username + ":" + password
	encoded := base64.RawStdEncoding.EncodeToString([]byte(contents))
	return agent.dag.SetSecret("git_auth_token", "Basic "+encoded)
}

func (agent *Agent) GithubClone(ctx context.Context, basicAuth *dagger.Secret, githubEvent *GithubEvent) (*dagger.Directory, string, error) {
	event, err := github.ParseWebHook(githubEvent.EventType, githubEvent.Payload)
	if err != nil {
		return nil, "", err
	}

	var (
		gitSha     string
		repository string
	)
	switch ghEvent := event.(type) {
	case *github.PullRequestEvent:
		gitSha = *ghEvent.PullRequest.Head.SHA
		repository = *ghEvent.Repo.FullName
	case *github.PushEvent:
		// NOTE: If users have `PushEvent` enabled in their lists of webhooks
		// then we receive duplicated events every time a commit is pushed to
		// a pull request. To simplify how pocketci works we need to choose
		// to handle only one of those events when this duplication happens.
		// The easiest way of doing this is to ignore all push events that are not
		// on the typical main branches (develop, main, master, trunk). This will
		// prevent users from creating workflows that are triggered based on commits
		// that happen against arbitrary branches. There are workarounds we can apply
		// but they will complicate the implementation and I would rather wait
		// until people request the feature
		gitSha = *ghEvent.After
		repository = *ghEvent.Repo.FullName
		ref := *ghEvent.Ref
		if ref != "main" && ref != "master" && ref != "develop" && ref != "trunk" {
			return nil, "", ErrPushEventIgnored
		}
	}

	return agent.dag.
		Git("https://github.com/"+repository, dagger.GitOpts{KeepGitDir: true}).
		WithAuthHeader(basicAuth).
		Commit(gitSha).
		Tree(), repository, nil
}

func (agent *Agent) HandleGithub(ctx context.Context, event *GithubEvent, basicAuth *dagger.Secret) (*dagger.Service, error) {
	repoDir, repository, err := agent.GithubClone(ctx, basicAuth, event)
	if err != nil {
		return nil, err
	}
	fullRepo := strings.Split(repository, "/")
	repo := fullRepo[len(fullRepo)-1]

	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}

	return WebhookContainer(agent.dag).
		WithEnvVariable("CACHE_BUST", time.Now().String()).
		WithDirectory("/"+repo, repoDir).
		WithWorkdir("/"+repo).
		WithNewFile("/payload.json", dagger.ContainerWithNewFileOpts{
			Contents: string(payload),
		}).
		WithExposedPort(9000).
		WithExec([]string{"/usr/local/bin/webhook", "-verbose", "-port", "9000", "-hooks", "hooks.yaml"}, dagger.ContainerWithExecOpts{ExperimentalPrivilegedNesting: true}).
		AsService(), nil
}

func WebhookContainer(c *dagger.Client) *dagger.Container {
	return c.Container().From("ubuntu:lunar").
		WithExec([]string{"sh", "-c", "apt update && apt install -y curl wget git"}).
		WithExec([]string{"wget", "-q", "https://github.com/adnanh/webhook/releases/download/2.8.1/webhook-linux-amd64.tar.gz"}).
		WithExec([]string{"tar", "-C", "/usr/local/bin", "--strip-components", "1", "-xf", "webhook-linux-amd64.tar.gz", "webhook-linux-amd64/webhook"}).
		WithExec([]string{"sh", "-c", fmt.Sprintf(`cd / && DAGGER_VERSION="%s" curl -L https://dl.dagger.io/dagger/install.sh | DAGGER_VERSION="%s" sh`, DaggerVersion, DaggerVersion)})
}