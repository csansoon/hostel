package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/csansoon/hostel/internal/protocol"
)

// prompt asks to confirm an action: a key of choices runs it; another key
// cancels.
type prompt struct {
	text    string
	choices map[string]action
	// queue is the queue order that a move saw; the view cancels the prompt
	// when the order changes.
	queue []string
}

// action is a request of the view; it returns the message to show.
type action func(ctx context.Context) (string, error)

// confirm asks "<question> [y/N]" for one action.
func confirm(question string, a action) *prompt {
	return &prompt{text: question + " [y/N]", choices: map[string]action{"y": a, "Y": a}}
}

// detailLeases are the leases that the details list: the leases on the room,
// or the queue of the platform.
func (v *view) detailLeases() []protocol.LeaseStatus {
	switch {
	case v.open == nil:
		return nil
	case v.open.room:
		if v.details == nil {
			return nil
		}
		return v.details.Leases
	}
	for _, q := range queuesOf(v.st) {
		if q.platform == v.open.name {
			return q.leases
		}
	}
	return nil
}

// selectedLease is the selected lease of the details (the first one when the
// selected lease is gone), or an empty lease.
func (v *view) selectedLease() protocol.LeaseStatus {
	leases := v.detailLeases()
	if i := slices.IndexFunc(leases, func(l protocol.LeaseStatus) bool { return l.ID == v.leaseSel }); i >= 0 {
		return leases[i]
	}
	if len(leases) > 0 {
		return leases[0]
	}
	return protocol.LeaseStatus{}
}

func (v *view) admin() bool { return v.st.Role == protocol.RoleAdmin }

// canEnd: an admin ends any lease, a member only the member's own leases (the
// server checks it again).
func (v *view) canEnd(l protocol.LeaseStatus) bool {
	return l.ID != "" && (v.admin() || (v.st.User != "" && l.User == v.st.User))
}

func (v *view) detailsFooter() string {
	parts := []string{"esc back"}
	leases := v.detailLeases()
	if len(leases) > 1 {
		parts = append(parts, "↑↓ select")
	}
	if v.canEnd(v.selectedLease()) {
		parts = append(parts, "e end lease")
	}
	if v.admin() {
		if v.open.room {
			if r, ok := v.openRoom(); ok {
				parts = append(parts, map[bool]string{false: "d drain", true: "d undrain"}[r.Drained], "r repair")
			}
		} else if len(leases) > 1 {
			parts = append(parts, "shift+↑/↓ (K/J) move", "p priority")
		} else if len(leases) == 1 {
			parts = append(parts, "p priority")
		}
	}
	return strings.Join(append(parts, "q quit"), " · ")
}

// detailsKey handles a key in the details of a room or a queue.
func (v *view) detailsKey(k string) {
	leases := v.detailLeases()
	l := v.selectedLease()
	i := slices.IndexFunc(leases, func(x protocol.LeaseStatus) bool { return x.ID == l.ID })
	switch k {
	case keyUp:
		if i > 0 {
			v.leaseSel = leases[i-1].ID
		}
	case keyDown:
		if i >= 0 && i < len(leases)-1 {
			v.leaseSel = leases[i+1].ID
		}
	case "e":
		if v.canEnd(l) {
			v.leaseSel = l.ID
			v.prompt = confirm(fmt.Sprintf("End lease %s of %s (%s)?", l.ID, l.User, l.Task), func(ctx context.Context) (string, error) {
				return "Lease " + l.ID + " ended.", v.c.Do(ctx, "POST", "/v1/leases/"+l.ID+"/release", nil, nil)
			})
		}
	case "d", "r":
		r, ok := v.openRoom()
		if !v.admin() || !ok {
			return
		}
		verb := "repair"
		var body any = map[string]bool{"accept_host_key": false}
		if k == "d" {
			verb, body = "drain", nil
			if r.Drained {
				verb = "undrain"
			}
		}
		v.prompt = confirm(fmt.Sprintf("%s room %s?", strings.ToUpper(verb[:1])+verb[1:], r.Name), func(ctx context.Context) (string, error) {
			return fmt.Sprintf("Room %s: %s requested.", r.Name, verb),
				v.c.Do(ctx, "POST", "/v1/rooms/"+strconv.FormatInt(r.ID, 10)+"/"+verb, body, nil)
		})
	case keyShiftUp, "K":
		if v.admin() && !v.open.room && i > 0 {
			v.leaseSel = l.ID
			v.prompt = v.move(l, leases[i-1].ID, fmt.Sprintf("Move lease %s (%s) up, before lease %s (%s)?", l.ID, l.User, leases[i-1].ID, leases[i-1].User), leases)
		}
	case keyShiftDown, "J":
		if v.admin() && !v.open.room && i >= 0 && i < len(leases)-1 {
			v.leaseSel = l.ID
			q := fmt.Sprintf("Move lease %s (%s) down, after lease %s (%s)?", l.ID, l.User, leases[i+1].ID, leases[i+1].User)
			if i+2 < len(leases) {
				v.prompt = v.move(l, leases[i+2].ID, q, leases)
			} else {
				// No lease after the next one: the next lease moves before
				// this one (it gets the fixed place).
				v.prompt = v.move(leases[i+1], l.ID, q, leases)
			}
		}
	case "p":
		if v.admin() && !v.open.room && l.ID != "" {
			v.leaseSel = l.ID
			p := &prompt{text: fmt.Sprintf("Priority of lease %s (now %s): h high · n normal · l low · another key cancels", l.ID, l.Priority),
				choices: map[string]action{}}
			for key, prio := range map[string]string{"h": "high", "n": "normal", "l": "low"} {
				p.choices[key] = func(ctx context.Context) (string, error) {
					return fmt.Sprintf("Lease %s: priority %s.", l.ID, prio),
						v.c.Do(ctx, "POST", "/v1/leases/"+l.ID+"/priority", protocol.PriorityRequest{Priority: prio}, nil)
				}
			}
			v.prompt = p
		}
	}
}

// move asks to move lease a before the lease before, in the queue order that
// the prompt shows.
func (v *view) move(a protocol.LeaseStatus, before, question string, queue []protocol.LeaseStatus) *prompt {
	p := confirm(question, func(ctx context.Context) (string, error) {
		return "Moved.", v.c.Do(ctx, "POST", "/v1/leases/"+a.ID+"/move", protocol.MoveRequest{Before: before}, nil)
	})
	p.queue = leaseIDs(queue)
	return p
}

func leaseIDs(leases []protocol.LeaseStatus) []string {
	var out []string
	for _, l := range leases {
		out = append(out, l.ID)
	}
	return out
}

// openRoom is the open room as the status shows it now.
func (v *view) openRoom() (protocol.RoomStatus, bool) {
	if v.open == nil || !v.open.room {
		return protocol.RoomStatus{}, false
	}
	i := slices.IndexFunc(v.st.Rooms, func(r protocol.RoomStatus) bool { return r.Name == v.open.name })
	if i < 0 {
		return protocol.RoomStatus{}, false
	}
	return v.st.Rooms[i], true
}

// run runs an action in the background; its message comes back on
// v.actions.
func (v *view) run(ctx context.Context, a action) {
	v.msg = "Working …"
	go func() {
		rctx, cancel := context.WithTimeout(ctx, statusTimeout)
		defer cancel()
		msg, err := a(rctx)
		if err != nil {
			msg = "Failed: " + err.Error()
		}
		select {
		case v.actions <- msg:
		case <-ctx.Done():
		}
	}()
}
