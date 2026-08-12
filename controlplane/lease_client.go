package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ClaimLease claims a service for this member, or renews it when this member
// already owns it. A live lease held by someone else answers ErrLeaseHeld with
// the owner named — the loser stands down and can say why (plan 30).
func (c *Client) ClaimLease(ctx context.Context, pin string, req LeaseRequest) (Lease, error) {
	lease, err := c.leaseCall(ctx, http.MethodPut, "/v1/sessions/"+pin+"/leases/"+req.Service, req)
	if err != nil {
		return lease, err
	}
	// Read-your-writes (plan 43): a claim changes the session document the
	// stream may not have pushed yet, so Session() must not hand placement a
	// pre-claim snapshot. The granted lease is the whole change — patch it in
	// rather than issuing a GET, which would put the request this plan exists
	// to remove back into the steady state.
	c.cacheLease(pin, lease)
	return lease, nil
}

// cacheLease folds one granted lease into the cached session document.
func (c *Client) cacheLease(pin string, lease Lease) {
	if lease.Service == "" {
		return
	}
	c.setCachedLease(pin, lease.Service, &lease)
}

// setCachedLease writes (or, with a nil lease, removes) one service's lease in
// the cached document. Leases are folded rather than invalidated because they
// are written on the hot path — placement claims and releases constantly, and a
// cache drop there would put a GET back into the steady state for every one.
func (c *Client) setCachedLease(pin, service string, lease *Lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sess, ok := c.cache[pin]
	if !ok {
		return // nothing cached yet: the first frame or snapshot carries it
	}
	leases := make(map[string]Lease, len(sess.Leases)+1)
	for k, v := range sess.Leases {
		if k == service {
			continue
		}
		leases[k] = v
	}
	if lease != nil {
		leases[service] = *lease
	}
	sess.Leases = leases
	c.cache[pin] = sess
}

// RenewLease extends a lease this member already owns. A member that has lost
// the lease is told so instead of silently re-taking it.
func (c *Client) RenewLease(ctx context.Context, pin string, req LeaseRequest) (Lease, error) {
	return c.leaseCall(ctx, http.MethodPost, "/v1/sessions/"+pin+"/leases/"+req.Service+"/renew", req)
}

// HandoverLease transfers a lease without waiting for expiry. With Req.To empty
// it expires the lease so the best-ranked member claims it next.
func (c *Client) HandoverLease(ctx context.Context, pin string, req LeaseRequest) (Lease, error) {
	lease, err := c.leaseCall(ctx, http.MethodPost, "/v1/sessions/"+pin+"/leases/"+req.Service+"/handover", req)
	if err == nil {
		if lease.Service == "" {
			c.setCachedLease(pin, req.Service, nil) // expired for the next claimer
		} else {
			c.cacheLease(pin, lease)
		}
	}
	return lease, err
}

// ReleaseLease gives a service up immediately — the "I am leaving" path.
// The actor is the Bearer (X-Slopball-Member); there is no memberId query
// parameter (abuse-surface ticket 19 / #22).
func (c *Client) ReleaseLease(ctx context.Context, pin, service string) error {
	_, err := c.leaseCall(ctx, http.MethodDelete,
		"/v1/sessions/"+pin+"/leases/"+service, nil)
	if err == nil {
		c.setCachedLease(pin, service, nil)
	}
	return err
}

// Leases reads the current placement from the session snapshot.
func (c *Client) Leases(ctx context.Context, pin string) (map[string]Lease, error) {
	sess, err := c.Session(ctx, pin)
	if err != nil {
		return nil, err
	}
	return sess.Leases, nil
}

// leaseCall is a thin sibling of do() that preserves the 409 body. do() maps
// every 409 to ErrDemoted, which is right for endpoint writes but would throw
// away the one thing a losing claimer needs: who holds the lease.
func (c *Client) leaseCall(ctx context.Context, method, path string, body any) (Lease, error) {
	var out Lease
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return out, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := newVersionedRequest(ctx, method, c.Base+path, rdr)
	if err != nil {
		return out, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.attachBearer(req, path)
	res, err := c.http().Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	switch {
	case res.StatusCode == http.StatusUpgradeRequired:
		return out, upgradeRequired(path, b)
	case res.StatusCode == 409:
		return out, fmt.Errorf("%w: %s", ErrLeaseHeld, strings.TrimSpace(trimLeaseErr(string(b))))
	case res.StatusCode == 204:
		return out, nil
	case res.StatusCode >= 300:
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = res.Status
		}
		return out, fmt.Errorf("%s: %s", path, msg)
	}
	if len(b) == 0 {
		return out, nil
	}
	return out, json.Unmarshal(b, &out)
}

// trimLeaseErr strips the server-side "lease held: " prefix so wrapping does
// not read as "lease held: lease held: …".
func trimLeaseErr(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), ErrLeaseHeld.Error()+": ")
}
