package daemon

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	relayv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/relay/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/pairing"
	"github.com/Luisotee/agent-mesh/daemon/internal/relay"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

const (
	pairingTokenPrefix = "amp_"
	defaultPairingTTL  = 10 * time.Minute
	maxPairingTTL      = 24 * time.Hour
)

func (d *Daemon) trackStream(actorID string, cancel context.CancelFunc) func() {
	d.streamsMu.Lock()
	d.nextStream++
	id := d.nextStream
	m := d.streams[actorID]
	if m == nil {
		m = map[uint64]context.CancelFunc{}
		d.streams[actorID] = m
	}
	m[id] = cancel
	d.streamsMu.Unlock()
	return func() {
		d.streamsMu.Lock()
		if m := d.streams[actorID]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(d.streams, actorID)
			}
		}
		d.streamsMu.Unlock()
	}
}

func (d *Daemon) disconnectActor(actorID string) {
	if d.rt != nil {
		d.rt.mu.Lock()
		for tk, t := range d.rt.tickets {
			if t.actorID == actorID {
				delete(d.rt.tickets, tk)
			}
		}
		d.rt.mu.Unlock()
	}
	d.streamsMu.Lock()
	m := d.streams[actorID]
	delete(d.streams, actorID)
	d.streamsMu.Unlock()
	for _, cancel := range m {
		cancel()
	}
}

// requireHostDevice allows only the host's own device; `what` completes the refusal ("only the host's own device may <what>").
func requireHostDevice(ctx context.Context, what string) error {
	actor := auth.ActorFrom(ctx)
	if actor == nil || actor.Id != localDeviceID {
		return denied("only the host's own device may %s", what)
	}
	return nil
}

// CreatePairing mints a one-time pairing token and QR/link payload. Only the host's own device, or the root of the
// setup run, may call it.
func (d *Daemon) CreatePairing(ctx context.Context, req *connect.Request[v1.CreatePairingRequest]) (*connect.Response[v1.CreatePairingResponse], error) {
	if err := d.requireHostOrSetup(ctx); err != nil {
		return nil, err
	}
	const maxPairingTTLSeconds = int64(maxPairingTTL / time.Second)
	ttl := defaultPairingTTL
	if req.Msg.TtlSeconds < 0 || req.Msg.TtlSeconds > maxPairingTTLSeconds {
		return nil, invalid("ttl_seconds must be between 0 and %d", maxPairingTTLSeconds)
	}
	if req.Msg.TtlSeconds > 0 {
		ttl = time.Duration(req.Msg.TtlSeconds) * time.Second
	}
	class := v1.ActorClass_ACTOR_CLASS_DEVICE
	if req.Msg.ReadOnly {
		class = v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE
	}
	relayURL := strings.TrimSpace(req.Msg.RelayUrl)
	cfgRelayURL := strings.TrimSpace(d.cfg.Relay.URL)
	var relayTCP string
	// ourRelay: the link names the relay this daemon is registered with, so it carries the device's token there.
	ourRelay := false
	if relayURL == "" {
		relayURL = cfgRelayURL
		relayTCP = d.cfg.Relay.TCP
		ourRelay = d.cfg.Relay.Token != ""
	} else if cfgRelayURL != "" && sameRelayURL(relayURL, cfgRelayURL) {
		relayTCP = d.cfg.Relay.TCP
		ourRelay = d.cfg.Relay.Token != ""
	}
	directURL := strings.TrimSpace(req.Msg.DirectUrl)
	if directURL == "" {
		directURL = d.tailnetDirectURL(ctx)
	}
	if directURL == "" && d.info.Listen != "" {
		if host, _, err := net.SplitHostPort(d.info.Listen); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
			directURL = "https://" + d.info.Listen
		}
	}
	if relayURL == "" && directURL == "" {
		// A link with no address in it can't be redeemed from anywhere: say why, so the user can give the host one.
		return nil, failedPrecondition("%s", d.noRouteCause())
	}

	out := &v1.CreatePairingResponse{}
	replayed, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceCreatePairingProcedure, out, func(tx *store.Tx) error {
		now := tx.Now()
		exp := now.Add(ttl)
		tok := ids.Secret(pairingTokenPrefix)
		pid := ids.New("p", 10)
		// The device's id is minted now, so the link can carry the device's own relay token: the relay accepts
		// it while the pairing is open and, once redeemed, until the device is revoked.
		devID := ids.New("dev", 10)
		var relayClientTok string
		if ourRelay {
			relayClientTok = relay.DeviceToken(d.cfg.Relay.Token, d.daemonID, devID)
		}
		name := strings.TrimSpace(req.Msg.Name)
		uri, err := pairing.EncodeURI(pairing.Payload{
			HostID:           d.daemonID,
			HostName:         d.hostName,
			CertFingerprint:  d.certFP,
			Token:            tok,
			RelayURL:         relayURL,
			RelayTCP:         relayTCP,
			RelayClientToken: relayClientTok,
			DirectURL:        directURL,
			DeviceName:       name,
		})
		if err != nil {
			return err
		}
		if err := tx.PutPairing(store.PairingRecord{
			Hash:       auth.Hash(tok),
			ID:         pid,
			Name:       name,
			Class:      class,
			CreatedAt:  now,
			ExpiresAt:  exp,
			DeviceID:   devID,
			MutationID: req.Msg.Meta.GetMutationId(),
			CreatedBy:  tx.Actor,
		}); err != nil {
			return err
		}
		out.PairingId = pid
		out.Token = tok
		out.HostId = d.daemonID
		out.HostName = d.hostName
		out.CertFingerprint = d.certFP
		out.RelayUrl = relayURL
		out.RelayTcp = relayTCP
		out.RelayClientToken = relayClientTok
		out.DirectUrl = directURL
		out.Uri = uri
		out.ExpiresAt = tsOf(exp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !replayed {
		d.relayTokensChanged()
	}
	if replayed && out.Token == "" {
		// The stored response was scrubbed when the pairing was redeemed or expired: there is no token or URI
		// left to hand back.
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("pairing already redeemed"))
	}
	return connect.NewResponse(out), nil
}

// noRouteCause says why this host has no address a device could pair over, and the ways to give it one.
func (d *Daemon) noRouteCause() string {
	why := "this host has no address a device could pair over: it serves no TLS address (meshd --listen <address>), has no relay (--relay-url), and"
	switch {
	case d.cfg.Tailnet != nil:
		why += " its Tailscale node isn't connected yet (see the tailnet's state: it may need a login)"
	default:
		why += " Tailscale is off (turn it on from the app's Pair screen, or start meshd with --tailscale)"
	}
	if d.info.Listen != "" {
		why += "; --listen " + d.info.Listen + " names no single address a device could reach, so give one with `direct_url`"
	}
	return why + ". Give the host one of them, or pass a `direct_url`."
}

// sameRelayURL reports whether two relay URLs name the same endpoint: scheme and host compare
// case-insensitively, the scheme's default port equals no port, and a trailing slash is ignored.
func sameRelayURL(a, b string) bool {
	norm := func(raw string) (string, bool) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			return "", false
		}
		scheme := strings.ToLower(u.Scheme)
		host := strings.ToLower(u.Hostname())
		port := u.Port()
		if (scheme == "https" || scheme == "wss") && port == "443" || (scheme == "http" || scheme == "ws") && port == "80" {
			port = ""
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		if port != "" {
			host += ":" + port
		}
		return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/"), true
	}
	na, oka := norm(a)
	nb, okb := norm(b)
	return oka && okb && na == nb
}

// RedeemPairing exchanges a one-time pairing token for a per-device bearer token.
func (d *Daemon) RedeemPairing(ctx context.Context, req *connect.Request[v1.RedeemPairingRequest]) (*connect.Response[v1.RedeemPairingResponse], error) {
	tok := strings.TrimSpace(req.Msg.Token)
	if tok == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing pairing token"))
	}
	var out *v1.RedeemPairingResponse
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		rec, err := tx.Pairing(auth.Hash(tok))
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("unknown pairing token"))
		}
		if err != nil {
			return err
		}
		if !rec.UsedAt.IsZero() {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("pairing token already used"))
		}
		now := tx.Now()
		if now.After(rec.ExpiresAt) {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("pairing token expired"))
		}
		name := strings.TrimSpace(req.Msg.DeviceName)
		if name == "" {
			name = rec.Name
		}
		if name == "" {
			name = "device"
		}
		devID := rec.DeviceID
		if devID == "" { // made before device ids were minted with the pairing
			devID = ids.New("dev", 10)
		}
		devTok := auth.NewToken()
		if err := tx.MarkPairingUsed(rec.Hash, devID); err != nil {
			return err
		}
		if err := tx.PutToken(auth.Hash(devTok), store.TokenRecord{
			Class:     rec.Class,
			SubjectID: devID,
			Name:      name,
		}); err != nil {
			return err
		}
		// Scrub secrets from the CreatePairing mutation record while keeping the idempotency tombstone so a retry
		// with the same mutation_id does not mint a fresh pairing token.
		if rec.MutationID != "" && rec.CreatedBy != nil {
			if err := tx.ScrubPairingMutation(rec.CreatedBy.Id, rec.MutationID); err != nil {
				return err
			}
		}
		dev := &v1.Device{
			Id:        devID,
			Name:      name,
			Class:     rec.Class,
			CreatedAt: tsOf(now),
		}
		tx.Actor = &v1.Actor{Class: rec.Class, Id: devID, Name: name}
		if _, err := tx.AppendEvent(&v1.Event{
			Payload: &v1.Event_DevicePaired{DevicePaired: &v1.DevicePaired{Device: dev, PairingId: rec.ID}},
		}); err != nil {
			return err
		}
		out = &v1.RedeemPairingResponse{
			DeviceId:        devID,
			DeviceName:      name,
			Class:           rec.Class,
			DeviceToken:     devTok,
			HostId:          d.daemonID,
			HostName:        d.hostName,
			CertFingerprint: d.certFP,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	d.relayTokensChanged()
	return connect.NewResponse(out), nil
}

// ListDevices lists paired devices. Only the host's own device, or the root of the setup run, may call it.
func (d *Daemon) ListDevices(ctx context.Context, req *connect.Request[v1.ListDevicesRequest]) (*connect.Response[v1.ListDevicesResponse], error) {
	if err := d.requireHostOrSetup(ctx); err != nil {
		return nil, err
	}
	devs, err := d.store.Read(ctx).ListDevices(req.Msg.IncludeRevoked, localDeviceID)
	if err != nil {
		return nil, err
	}
	regs, err := d.store.Read(ctx).ListPush()
	if err != nil {
		return nil, err
	}
	for _, dev := range devs {
		for _, r := range regs {
			if r.DeviceID == dev.Id {
				dev.Push = r.Proto()
			}
		}
	}
	return connect.NewResponse(&v1.ListDevicesResponse{Devices: devs}), nil
}

// RevokeDevice revokes a paired device's token and immediately closes its active streams.
func (d *Daemon) RevokeDevice(ctx context.Context, req *connect.Request[v1.RevokeDeviceRequest]) (*connect.Response[v1.RevokeDeviceResponse], error) {
	if err := requireHostDevice(ctx, "manage paired devices"); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(req.Msg.DeviceId)
	if id == "" {
		return nil, invalid("no device_id given")
	}
	out := &v1.RevokeDeviceResponse{}
	if err := d.revokeDevice(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRevokeDeviceProcedure, id, out, func(dev *v1.Device) { out.Device = dev }); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// RevokeSelf revokes the calling device's own token and closes its streams: how a phone unpairs itself, since it may
// not call RevokeDevice. The host's own device can't be revoked.
func (d *Daemon) RevokeSelf(ctx context.Context, req *connect.Request[v1.RevokeSelfRequest]) (*connect.Response[v1.RevokeSelfResponse], error) {
	actor := auth.ActorFrom(ctx)
	if actor == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no actor"))
	}
	out := &v1.RevokeSelfResponse{}
	if err := d.revokeDevice(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRevokeSelfProcedure, actor.Id, out, func(dev *v1.Device) { out.Device = dev }); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// revokeDevice revokes device id's tokens, records DeviceRevoked, tells the relay and closes the device's streams.
// out is the response the write records for a retry; set puts the revoked device into it, inside the write.
func (d *Daemon) revokeDevice(ctx context.Context, meta *v1.RequestMeta, procedure, id string, out proto.Message, set func(*v1.Device)) error {
	if id == localDeviceID {
		return invalid("cannot revoke the host's local device")
	}
	_, err := d.write(ctx, meta, procedure, out, func(tx *store.Tx) error {
		dev, err := tx.GetDevice(id)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("no device %s", id)
		}
		if err != nil {
			return err
		}
		if dev.RevokedAt == nil {
			if err := tx.RevokeTokens(id); err != nil {
				return err
			}
			if _, err := tx.DeletePush(id, ""); err != nil {
				return err
			}
			dev.RevokedAt = tsOf(tx.Now())
			if _, err := tx.AppendEvent(&v1.Event{
				Payload: &v1.Event_DeviceRevoked{DeviceRevoked: &v1.DeviceRevoked{DeviceId: dev.Id, Name: dev.Name}},
			}); err != nil {
				return err
			}
		}
		set(dev)
		return nil
	})
	if err != nil {
		return err
	}
	d.relayTokensChanged()
	d.disconnectActor(id)
	d.push.stop(id, "")
	return nil
}

// relayTokensChanged tells the relay connector the devices' relay tokens changed (a pairing made, redeemed or
// pruned, a device revoked), so the relay hears the new set at once.
func (d *Daemon) relayTokensChanged() {
	if d.relay != nil {
		d.relay.TokensChanged()
	}
}

// relayClientTokens is the set of client tokens the relay may accept for this host: every open pairing until it
// expires (first, so a host at the relay's cap can still pair), then every paired device not revoked.
func (d *Daemon) relayClientTokens() ([]*relayv1.ClientToken, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := d.store.Read(ctx)
	now := d.now()
	open, err := r.OpenPairings(now)
	if err != nil {
		return nil, err
	}
	devs, err := r.ListDevices(false, localDeviceID)
	if err != nil {
		return nil, err
	}
	entry := func(devID string, ttl int64) *relayv1.ClientToken {
		h := relay.TokenHash(relay.DeviceToken(d.cfg.Relay.Token, d.daemonID, devID))
		return &relayv1.ClientToken{Sha256: h[:], TtlSeconds: ttl}
	}
	out := make([]*relayv1.ClientToken, 0, len(devs)+len(open))
	for _, p := range open {
		// Rounded up, and at least 1: 0 would mean no expiry.
		out = append(out, entry(p.DeviceID, max(1, int64((p.ExpiresAt.Sub(now)+time.Second-1)/time.Second))))
	}
	for _, dev := range devs {
		out = append(out, entry(dev.Id, 0))
	}
	return out, nil
}
