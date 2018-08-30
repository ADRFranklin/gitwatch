// Package gitwatch provides a simple tool to first clone a set of git
// repositories to a local directory and then periodically check them all for
// any updates.
package gitwatch

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
)

// Session represents a git watch session configuration
type Session struct {
	Repositories []string             // list of local or remote repository URLs to watch
	Interval     time.Duration        // the interval between remote checks
	Directory    string               // the directory to store repositories
	Auth         transport.AuthMethod // authentication method for git operations
	InitialEvent bool                 // if true, an event for each repo will be emitted upon construction
	InitialDone  chan struct{}        // if InitialEvent true, this is pushed to after initial setup done
	Events       chan Event           // when a change is detected, events are pushed here
	Errors       chan error           // when an error occurs, errors come here instead of halting the loop

	ctx context.Context
	cf  context.CancelFunc
}

// Event represents an update detected on one of the watched repositories
type Event struct {
	URL       string
	Path      string
	Timestamp time.Time
}

// New constructs a new git watch session on the given repositories
func New(
	ctx context.Context,
	repos []string,
	interval time.Duration,
	dir string,
	auth transport.AuthMethod,
	initialEvent bool,
) (session *Session, err error) {
	ctx2, cf := context.WithCancel(ctx)
	session = &Session{
		Repositories: repos,
		Interval:     interval,
		Directory:    dir,
		Events:       make(chan Event),
		Errors:       make(chan error, 16),
		InitialEvent: initialEvent,
		InitialDone:  make(chan struct{}, 1),

		ctx: ctx2,
		cf:  cf,
	}
	return
}

// Run begins the watcher and blocks until an error occurs
func (s *Session) Run() (err error) {
	return s.daemon()
}

// Close gracefully shuts down the git watcher
func (s *Session) Close() {
	s.cf()
}

func (s *Session) daemon() (err error) {
	t := time.NewTicker(s.Interval)

	// a function to select over the session's context and the ticker to check
	// repositories.
	f := func() (err error) {
		select {
		case <-s.ctx.Done():
			err = context.Canceled
		case <-t.C:
			err = s.checkRepos()
			if err != nil {
				s.Errors <- err
				return nil
			}
		}
		return
	}

	// before starting the daemon process loop, perform an initial check against
	// all targets. If the targets do not exist, they will be cloned and events
	// will be emitted for them.
	if s.InitialEvent {
		err = s.checkRepos()
		if err != nil {
			return
		}
		s.InitialDone <- struct{}{}
	}

	for {
		err = f()
		if err != nil {
			return
		}
	}
}

// checkRepos simply iterates all repositories and collects events from them, if
// there are any, they will be emitted to the Events channel concurrently.
func (s *Session) checkRepos() (err error) {
	for _, repoPath := range s.Repositories {
		var event *Event
		event, err = s.checkRepo(repoPath)
		if err != nil {
			return
		}

		if event != nil {
			go func() { s.Events <- *event }()
		}
	}
	return
}

// checkRepo checks a specific git repository that may or may not exist locally
// and if there are changes or the repository had to be cloned fresh (and
// InitialEvents is true) then an event is returned.
func (s *Session) checkRepo(repoPath string) (event *Event, err error) {
	localPath, err := GetRepoPath(s.Directory, repoPath)
	if err != nil {
		err = errors.Wrap(err, "failed to get path from repo url")
		return
	}

	repo, err := git.PlainOpen(localPath)
	if err != nil {
		if err != git.ErrRepositoryNotExists {
			err = errors.Wrap(err, "failed to open local repo")
			return
		}

		return s.cloneRepo(repoPath, localPath)
	}

	return GetEventFromRepoChanges(repo)
}

// cloneRepo clones the specified repository to the session's cache and, if
// InitialEvent is true, emits an event for the newly cloned repo.
func (s *Session) cloneRepo(repoPath, localPath string) (event *Event, err error) {
	repo, err := git.PlainCloneContext(s.ctx, localPath, false, &git.CloneOptions{
		Auth: s.Auth,
		URL:  repoPath,
	})
	if err != nil {
		err = errors.Wrap(err, "failed to clone initial copy of repository")
		return
	}

	if s.InitialEvent {
		event, err = GetEventFromRepo(repo)
	}
	return
}

// GetEventFromRepoChanges reads a locally cloned git repository an returns an
// event only if an attempted fetch resulted in new changes in the working tree.
func GetEventFromRepoChanges(repo *git.Repository) (event *Event, err error) {
	wt, err := repo.Worktree()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get worktree")
	}

	err = wt.Pull(&git.PullOptions{})
	if err != nil {
		if err == git.NoErrAlreadyUpToDate {
			return nil, nil
		}
		return nil, errors.Wrap(err, "failed to pull local repo")
	}

	return GetEventFromRepo(repo)
}

// GetEventFromRepo reads a locally cloned git repository and returns an event
// based on the most recent commit.
func GetEventFromRepo(repo *git.Repository) (event *Event, err error) {
	wt, err := repo.Worktree()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get worktree")
	}
	remote, err := repo.Remote("origin")
	if err != nil {
		return
	}
	ref, err := repo.Head()
	if err != nil {
		return
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return
	}
	return &Event{
		URL:       remote.Config().URLs[0],
		Path:      wt.Filesystem.Root(),
		Timestamp: c.Author.When,
	}, nil
}

// GetRepoPath returns the local path of a cached repo from the given cache, the
// base component of the repo path is used as a directory name for the target
// repository.
func GetRepoPath(cache, repo string) (result string, err error) {
	path := strings.Split(repo, ":")
	i := 0
	if len(path) == 2 {
		i = 1
	}
	u, err := url.Parse(path[i])
	if err != nil {
		return
	}
	return filepath.Join(cache, filepath.Base(u.Path)), nil
}
