package services

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/go-pg/pg/v10"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	mp "github.com/webtor-io/webhook/models/patreon"

	"crypto/hmac"
	"crypto/md5"
)

const (
	patreonSecretFlag        = "patreon-secret"
	refreshTimeoutFlag       = "patreon-refresh-timeout"
	refreshRetriesFlag       = "patreon-refresh-retries"
	refreshRetryDelayFlag    = "patreon-refresh-retry-delay"
	refreshMembersStmt       = `REFRESH MATERIALIZED VIEW CONCURRENTLY patreon."member"`
	defaultRefreshTimeout    = 10 * time.Minute
	defaultRefreshRetries    = 2
	defaultRefreshRetryDelay = 30 * time.Second
	// memberRefreshLockKey is an arbitrary fixed key for the session-level
	// advisory lock that serialises patreon.member refreshes across processes
	// (the realtime webhook pod and the periodic refresh-members cron). It keeps
	// the two from queuing behind each other on the matview's own CONCURRENTLY
	// lock and blowing the refresh timeout under load.
	memberRefreshLockKey int64 = 0x70617472 // "patr"
)

func RegisterPatreonFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   patreonSecretFlag,
			Usage:  "patreon secret",
			Value:  "",
			EnvVar: "PATREON_SECRET",
		},
		cli.DurationFlag{
			Name:   refreshTimeoutFlag,
			Usage:  "timeout for REFRESH MATERIALIZED VIEW CONCURRENTLY patreon.member; raise if the view grows large enough to exceed it",
			Value:  defaultRefreshTimeout,
			EnvVar: "PATREON_REFRESH_TIMEOUT",
		},
		cli.IntFlag{
			Name:   refreshRetriesFlag,
			Usage:  "extra attempts for the periodic patreon.member refresh after a failed one (0 disables retries)",
			Value:  defaultRefreshRetries,
			EnvVar: "PATREON_REFRESH_RETRIES",
		},
		cli.DurationFlag{
			Name:   refreshRetryDelayFlag,
			Usage:  "delay between patreon.member refresh attempts",
			Value:  defaultRefreshRetryDelay,
			EnvVar: "PATREON_REFRESH_RETRY_DELAY",
		},
	)
}

type Patreon struct {
	db     *cs.PG
	secret string
	nats   *cs.NATS

	refreshTimeout    time.Duration
	refreshRetries    int
	refreshRetryDelay time.Duration

	refreshMu      sync.Mutex
	refreshRunning bool
	refreshWaiters []chan struct{}
}

func NewPatreon(c *cli.Context, db *cs.PG, nats *cs.NATS) *Patreon {
	to := c.Duration(refreshTimeoutFlag)
	if to <= 0 {
		to = defaultRefreshTimeout
	}
	retries := c.Int(refreshRetriesFlag)
	if retries < 0 {
		retries = 0
	}
	delay := c.Duration(refreshRetryDelayFlag)
	if delay <= 0 {
		delay = defaultRefreshRetryDelay
	}
	return &Patreon{
		secret:            c.String(patreonSecretFlag),
		db:                db,
		nats:              nats,
		refreshTimeout:    to,
		refreshRetries:    retries,
		refreshRetryDelay: delay,
	}
}

func (s *Patreon) Handle(w http.ResponseWriter, r *http.Request) {
	db := s.db.Get()
	b := r.Body
	defer b.Close()
	bb, err := ioutil.ReadAll(b)
	if err != nil {
		log.WithError(err).Error("failed to read request body")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	event := r.Header.Get("X-Patreon-Event")
	signature := r.Header.Get("X-Patreon-Signature")
	log.
		WithField("payload_len", len(bb)).
		WithField("event", event).
		WithField("signature", signature).
		Info("request received")
	var p mp.Payload
	err = json.Unmarshal(bb, &p)
	if err != nil {
		log.WithError(err).Errorf("failed to unmarshal request body=%v", bb)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ds, err := hex.DecodeString(signature)
	if err != nil {
		log.WithError(err).Errorf("failed to decode signature=%v", signature)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !s.validate(bb, ds, []byte(s.secret)) {
		log.Warn("signature validation failed")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	m := &mp.Message{
		Payload:   p,
		Event:     event,
		Signature: ds,
	}
	_, err = db.Model(m).Insert()
	if err != nil {
		log.WithError(err).Errorf("failed to store patreon message=%+v", m)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.WithField("message", m).Info("message stored")
	w.WriteHeader(http.StatusOK)
	s.scheduleRefreshAndPublish(p, event)
}

// scheduleRefreshAndPublish queues a background refresh of patreon.member and
// publishes user.updated once the refresh completes. Concurrent calls coalesce
// into at most one running + one pending refresh.
func (s *Patreon) scheduleRefreshAndPublish(p mp.Payload, event string) {
	done := make(chan struct{})

	s.refreshMu.Lock()
	s.refreshWaiters = append(s.refreshWaiters, done)
	startWorker := !s.refreshRunning
	s.refreshRunning = true
	s.refreshMu.Unlock()

	if startWorker {
		go s.refreshLoop()
	}
	go func() {
		<-done
		s.publish(p, event)
	}()
}

func (s *Patreon) refreshLoop() {
	for {
		// Snapshot the waiters BEFORE the refresh: only requests whose message
		// rows were inserted before this refresh starts are guaranteed to be
		// reflected in the refreshed matview. Waiters that arrive while the
		// refresh runs must not be signalled by it — they inserted after the
		// refresh's snapshot and are handled by the next iteration.
		s.refreshMu.Lock()
		waiters := s.refreshWaiters
		s.refreshWaiters = nil
		s.refreshMu.Unlock()

		if err := s.RefreshMembers(context.Background()); err != nil {
			log.WithError(err).Error("failed to refresh patreon.member")
		} else {
			log.Info("patreon.member refreshed")
		}

		for _, w := range waiters {
			close(w)
		}

		s.refreshMu.Lock()
		if len(s.refreshWaiters) == 0 {
			s.refreshRunning = false
			s.refreshMu.Unlock()
			return
		}
		s.refreshMu.Unlock()
	}
}

// RefreshMembers runs REFRESH MATERIALIZED VIEW CONCURRENTLY on patreon.member,
// waiting for any in-flight refresh to finish first. Used by the realtime
// webhook path, which must observe the message it just inserted before it
// publishes user.updated.
func (s *Patreon) RefreshMembers(ctx context.Context) error {
	_, err := s.refreshMembers(ctx, true)
	return err
}

// RefreshMembersIfIdle refreshes patreon.member only when no other refresh is
// running; otherwise it skips and returns false. Used by the periodic
// refresh-members cron so it never queues behind the realtime refresh on the
// matview's CONCURRENTLY lock (the in-flight refresh already brings the view up
// to date).
func (s *Patreon) RefreshMembersIfIdle(ctx context.Context) (bool, error) {
	return s.refreshMembers(ctx, false)
}

// RefreshMembersIfIdleWithRetry is RefreshMembersIfIdle plus retries, so a
// single slow refresh or dropped PG connection doesn't fail the whole cron
// job. Each attempt gets its own refreshTimeout window; a skip (another
// refresh in progress) returns immediately without retrying.
func (s *Patreon) RefreshMembersIfIdleWithRetry(ctx context.Context) (bool, error) {
	var (
		ran bool
		err error
	)
	for attempt := 0; ; attempt++ {
		ran, err = s.refreshMembers(ctx, false)
		if err == nil || attempt >= s.refreshRetries {
			return ran, err
		}
		log.WithError(err).
			WithField("attempt", attempt+1).
			WithField("max_attempts", s.refreshRetries+1).
			Warn("patreon.member refresh failed, retrying")
		select {
		case <-ctx.Done():
			return false, err
		case <-time.After(s.refreshRetryDelay):
		}
	}
}

// refreshMembers serialises patreon.member refreshes across processes with a
// session-level advisory lock held on a single pooled connection. With wait,
// it blocks until the lock is free; without, it tries the lock and returns
// (false, nil) when another refresh holds it.
func (s *Patreon) refreshMembers(ctx context.Context, wait bool) (bool, error) {
	db := s.db.Get()
	if db == nil {
		return false, errors.New("db not available")
	}
	ctx, cancel := context.WithTimeout(ctx, s.refreshTimeout)
	defer cancel()

	// REFRESH ... CONCURRENTLY cannot run inside a transaction, so the advisory
	// lock and the refresh must share one connection from the pool.
	conn := db.Conn()
	defer conn.Close()

	if wait {
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(?)", memberRefreshLockKey); err != nil {
			return false, errors.Wrap(err, "failed to acquire member refresh lock")
		}
	} else {
		var locked bool
		if _, err := conn.QueryOneContext(ctx, pg.Scan(&locked), "SELECT pg_try_advisory_lock(?)", memberRefreshLockKey); err != nil {
			return false, errors.Wrap(err, "failed to try member refresh lock")
		}
		if !locked {
			return false, nil
		}
	}
	// Release on the same connection with a fresh context so a refresh that hit
	// its timeout still unlocks before the connection returns to the pool.
	defer func() {
		if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(?)", memberRefreshLockKey); err != nil {
			// A conn broken by a failed refresh can't run the unlock, but the
			// session-level lock is released server-side when conn.Close ends
			// the session, so this is not a stuck-lock condition.
			log.WithError(err).Warn("failed to release member refresh lock (lock released by connection close)")
		}
	}()

	if _, err := conn.ExecContext(ctx, refreshMembersStmt); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Patreon) publish(p mp.Payload, event string) {
	publishUserUpdated(s.nats, userUpdatedFromPatreon(p, event))
}

// userUpdatedFromPatreon lifts the membership facts a consumer may act on
// out of the raw webhook payload. Missing or oddly typed attributes are
// simply left empty — the payload is Patreon's to shape, and the event must
// still go out for the email alone.
func userUpdatedFromPatreon(p mp.Payload, event string) UserUpdated {
	attrs := patreonAttrs(p)
	msg := UserUpdated{
		Email:          stringAttr(attrs, "email"),
		Source:         "patreon",
		Event:          event,
		PatronStatus:   stringAttr(attrs, "patron_status"),
		NextChargeDate: stringAttr(attrs, "next_charge_date"),
	}
	if v, ok := attrs["is_free_trial"].(bool); ok {
		msg.IsFreeTrial = &v
	}
	return msg
}

func patreonAttrs(p mp.Payload) map[string]interface{} {
	data, ok := p["data"].(map[string]interface{})
	if !ok {
		return nil
	}
	attrs, _ := data["attributes"].(map[string]interface{})
	return attrs
}

func stringAttr(attrs map[string]interface{}, key string) string {
	v, _ := attrs[key].(string)
	return v
}

func (s *Patreon) getEmail(p mp.Payload) string {
	return stringAttr(patreonAttrs(p), "email")
}

func (s *Patreon) Close() {
}

func (s *Patreon) validate(message, messageMAC, key []byte) bool {
	mac := hmac.New(md5.New, key)
	mac.Write(message)
	expectedMAC := mac.Sum(nil)
	return hmac.Equal(messageMAC, expectedMAC)
}
