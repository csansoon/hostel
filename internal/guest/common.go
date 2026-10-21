package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// Exit codes (design, section 10).
const (
	ExitError       = 1
	ExitUsage       = 2
	ExitUnavailable = 69 // no machine can satisfy the request
	ExitNotFinished = 75 // run the command that the last line names
)

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "hostel:", err)
	return ExitError
}

// lease is the context of a command that uses an existing lease.
type lease struct {
	repo *Repo
	st   *State
	api  *client.Client
}

func apiClient() (*client.Client, error) {
	g, err := config.LoadGuest()
	if err != nil {
		return nil, err
	}
	return client.New(g), nil
}

// openLease selects the lease of the worktree and records guest activity.
func openLease(platform string, stderr io.Writer) (*lease, int) {
	repo, err := FindRepo()
	if err != nil {
		return nil, fail(stderr, err)
	}
	st, err := repo.SelectState(platform)
	if err != nil {
		return nil, fail(stderr, err)
	}
	api, err := apiClient()
	if err != nil {
		return nil, fail(stderr, err)
	}
	l := &lease{repo: repo, st: st, api: api}
	if err := l.touch(); err != nil {
		return nil, fail(stderr, err)
	}
	if st.Access == nil {
		return nil, fail(stderr, fmt.Errorf("the %s lease is not ready yet; run \"hostel acquire %s --task ...\" again", st.Platform, st.Platform))
	}
	return l, 0
}

// touch records guest activity. A lease that the server no longer knows is
// marked final in the local state.
func (l *lease) touch() error {
	err := l.api.Do(context.Background(), "POST", "/v1/leases/"+l.st.LeaseID+"/touch", nil, nil)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Code == protocol.CodeLeaseEnded) {
		l.st.Final = true
		l.st.Save()
		return fmt.Errorf("%s; run \"hostel acquire %s --task ...\" for a new lease", apiErr.Message, l.st.Platform)
	}
	return err
}
