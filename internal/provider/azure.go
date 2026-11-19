package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
)

// azureAPIVersion is the Compute API version of the calls.
const azureAPIVersion = "2024-07-01"

// azureRequestTimeout bounds one HTTP request to Azure.
const azureRequestTimeout = 30 * time.Second

// Power states that List reports for Azure machines.
const (
	PowerRunning  = "running"
	PowerStopped  = "stopped"  // deallocated
	PowerStarting = "starting" // also a machine that is stopped but not deallocated
	PowerStopping = "stopping"
)

// Azure starts and deallocates existing Azure machines (M12). Access is the
// same as for a static machine.
type Azure struct {
	*Static
	cfg        *config.Provider
	secretFile string
	login      string
	management string
	http       *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewAzure reads the client secret from secretFile at each sign-in, so a new
// secret applies without a new provider.
func NewAzure(cfg *config.Provider, secretFile string, serverKey ssh.Signer, dialMachine MachineDialer) *Azure {
	a := &Azure{
		Static:     NewStatic(cfg, serverKey, dialMachine),
		cfg:        cfg,
		secretFile: secretFile,
		login:      strings.TrimSuffix(cfg.LoginURL, "/"),
		management: strings.TrimSuffix(cfg.ManagementURL, "/"),
		http:       &http.Client{Timeout: azureRequestTimeout},
	}
	if a.login == "" {
		a.login = "https://login.microsoftonline.com"
	}
	if a.management == "" {
		a.management = "https://management.azure.com"
	}
	return a
}

// SignOut drops the access token, so that the next call signs in with the
// secret that is in the file now.
func (a *Azure) SignOut() {
	a.mu.Lock()
	a.token = ""
	a.mu.Unlock()
}

func (a *Azure) Kind() string                     { return config.ProviderAzure }
func (a *Azure) Capabilities(string) Capabilities { return Capabilities{CanStop: true} }

// Resume starts the machine and waits until Azure reports the start done.
func (a *Azure) Resume(ctx context.Context, m MachineRef) error {
	return a.action(ctx, m, "start")
}

// Stop deallocates the machine (a machine that is only stopped is still
// billed) and waits until Azure reports it done.
func (a *Azure) Stop(ctx context.Context, m MachineRef) error {
	return a.action(ctx, m, "deallocate")
}

// List reports the power state of each machine of the provider (Name and
// Template are the machine's key). The prefix does not apply: Hostel did not
// create these machines.
func (a *Azure) List(ctx context.Context, _ string) ([]MachineRef, error) {
	var out []MachineRef
	for _, mn := range a.cfg.TemplateOrder {
		m := a.cfg.Templates[mn]
		if m == nil {
			continue
		}
		var view struct {
			Statuses []struct {
				Code string `json:"code"`
			} `json:"statuses"`
		}
		if _, err := a.call(ctx, http.MethodGet, a.management+m.VM+"/instanceView?api-version="+azureAPIVersion, &view); err != nil {
			return nil, fmt.Errorf("machine %s: %w", mn, err)
		}
		state := ""
		for _, st := range view.Statuses {
			if p, ok := strings.CutPrefix(st.Code, "PowerState/"); ok {
				switch p {
				case "running":
					state = PowerRunning
				case "deallocated":
					state = PowerStopped
				case "starting", "stopped":
					state = PowerStarting
				case "deallocating", "stopping":
					state = PowerStopping
				}
			}
		}
		out = append(out, MachineRef{Name: mn, Template: mn, State: state})
	}
	return out, nil
}

func (a *Azure) action(ctx context.Context, ref MachineRef, op string) error {
	m := a.cfg.Templates[ref.Template]
	if m == nil {
		return fmt.Errorf("machine %q is not in the configuration", ref.Template)
	}
	resp, err := a.call(ctx, http.MethodPost, a.management+m.VM+"/"+op+"?api-version="+azureAPIVersion, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	poll := resp.Header.Get("Azure-AsyncOperation")
	if poll == "" {
		poll = resp.Header.Get("Location")
	}
	if resp.StatusCode != http.StatusAccepted || poll == "" {
		return nil
	}
	if err := a.sameHost(poll); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	wait := retryAfter(resp.Header, 5*time.Second)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: Azure did not finish the operation in time: %w", op, ctx.Err())
		case <-time.After(wait):
		}
		var st struct {
			Status string `json:"status"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		r, err := a.call(ctx, http.MethodGet, poll, &st)
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		switch strings.ToLower(st.Status) {
		case "succeeded":
			return nil
		case "failed", "canceled":
			if st.Error != nil {
				return fmt.Errorf("%s: Azure: %s: %s", op, st.Error.Code, st.Error.Message)
			}
			return fmt.Errorf("%s: Azure: %s", op, st.Status)
		case "":
			// A Location URL answers 202 while it runs and 200 at the end.
			if r.StatusCode == http.StatusOK {
				return nil
			}
		}
		wait = retryAfter(r.Header, 5*time.Second)
	}
}

// sameHost refuses an operation URL on another host than the management
// address: the bearer token goes with each poll.
func (a *Azure) sameHost(raw string) error {
	u, err := url.Parse(raw)
	base, _ := url.Parse(a.management)
	if err != nil || base == nil || u.Scheme != base.Scheme || u.Host != base.Host {
		return fmt.Errorf("Azure gave an operation address on another host: %s", raw)
	}
	return nil
}

// call sends one request to the management API, with retries for 429 and
// 5xx inside ctx, and decodes a 2xx JSON body into out.
func (a *Azure) call(ctx context.Context, method, u string, out any) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		token, err := a.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if method == http.MethodPost {
			req.Header.Set("Content-Length", "0")
		}
		resp, err := a.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Azure request: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("Azure request: %w", err)
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out != nil && len(body) > 0 {
				if err := json.Unmarshal(body, out); err != nil {
					return nil, fmt.Errorf("Azure answer: %w", err)
				}
			}
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			// An access token that Azure no longer accepts: sign in again.
			a.SignOut()
			continue
		case (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < 5:
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryAfter(resp.Header, time.Duration(attempt+1)*2*time.Second)):
			}
			continue
		}
		err = azureError(resp.StatusCode, body)
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
		}
		return nil, err
	}
}

// accessToken signs in with the client secret (client credentials) and keeps
// the token until a minute before it expires.
func (a *Azure) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Now().Before(a.expires) {
		return a.token, nil
	}
	secret, err := os.ReadFile(a.secretFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no client secret for provider %s: run \"hostel config providers login %s\"", a.cfg.Name, a.cfg.Name)
	}
	if err != nil {
		return "", fmt.Errorf("read the client secret: %w", err)
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.cfg.ClientID},
		"client_secret": {strings.TrimSpace(string(secret))},
		"scope":         {a.management + "/.default"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.login+"/"+url.PathEscape(a.cfg.Tenant)+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("Azure sign-in: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("Azure sign-in: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		json.Unmarshal(body, &e)
		// The first line only: the rest is trace and correlation IDs.
		desc, _, _ := strings.Cut(e.Description, "\r\n")
		return "", fmt.Errorf("Azure sign-in failed (HTTP %d): %s %s", resp.StatusCode, e.Error, desc)
	}
	var t struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return "", errors.New("Azure sign-in: the answer has no access token")
	}
	secs, _ := strconv.Atoi(strings.Trim(string(t.ExpiresIn), `"`))
	a.token, a.expires = t.AccessToken, time.Now().Add(time.Duration(secs)*time.Second-time.Minute)
	return a.token, nil
}

func azureError(status int, body []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Code != "" {
		return fmt.Errorf("Azure: HTTP %d: %s: %s", status, e.Error.Code, e.Error.Message)
	}
	return fmt.Errorf("Azure: HTTP %d", status)
}

// retryAfter reads the Retry-After header (seconds), bounded to 1 minute.
func retryAfter(h http.Header, def time.Duration) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, time.Minute)
	}
	return def
}
