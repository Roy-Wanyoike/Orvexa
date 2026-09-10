package freeswitch

import (
        "context"
        "errors"
        "testing"
        "time"
)

// The tests below drive the real eslClient against the wire-faithful
// fakeSwitch: handshake variants, command round-trips, event delivery,
// switch-reboot reconnect and bounded command timeouts.

// testClientConfig returns a fast-failing client config for tests.
func testClientConfig(fs *fakeSwitch) clientConfig {
        return clientConfig{
                addr:        fs.addr(),
                password:    "ClueCon",
                events:      []string{"CHANNEL_ANSWER", "CHANNEL_HANGUP", "BACKGROUND_JOB"},
                dialTimeout: time.Second,
                hsTimeout:   2 * time.Second,
                cmdTimeout:  2 * time.Second,
                idleTimeout: 5 * time.Second,
                backoffBase: 10 * time.Millisecond,
                backoffMax:  50 * time.Millisecond,
        }
}

// waitReady polls until the client reports an established session.
func waitReady(t *testing.T, c *eslClient, timeout time.Duration) {
        t.Helper()
        deadline := time.Now().Add(timeout)
        for {
                if c.Ready() {
                        return
                }
                if time.Now().After(deadline) {
                        t.Fatal("client did not become ready in time (handshake/subscribe failed?)")
                }
                time.Sleep(5 * time.Millisecond)
        }
}

func TestESLClientHandshakeAndSubscription(t *testing.T) {
        t.Run("plaintext auth (stock mod_event_socket)", func(t *testing.T) {
                fs := newFakeSwitch(t)
                c := newESLClient(testClientConfig(fs))
                defer c.Close()
                waitReady(t, c, 2*time.Second)
                if got := fs.subscribedEvents(); got != "CHANNEL_ANSWER CHANNEL_HANGUP BACKGROUND_JOB" {
                        t.Fatalf("subscription line = %q", got)
                }
        })

        t.Run("challenge md5 auth (advertised hardening)", func(t *testing.T) {
                fs := newFakeSwitch(t)
                fs.challenge = "challenge-1772509797"
                c := newESLClient(testClientConfig(fs))
                defer c.Close()
                waitReady(t, c, 2*time.Second)
        })
}

func TestESLClientAuthRejected(t *testing.T) {
        fs := newFakeSwitch(t)
        cfg := testClientConfig(fs)
        cfg.password = "wrong-password"
        c := newESLClient(cfg)
        defer c.Close()

        ctx, cancel := context.WithTimeout(context.Background(), time.Second)
        defer cancel()
        _, err := c.Do(ctx, "api status")
        if err == nil {
                t.Fatal("command against an endpoint that rejects our credentials must fail")
        }
        if !errors.Is(err, errAuthRejected) {
                t.Fatalf("auth failure must classify as errAuthRejected, got: %v", err)
        }
        if c.Ready() {
                t.Fatal("client must not report ready while authentication is rejected")
        }
}

func TestESLClientCommandRoundTrip(t *testing.T) {
        fs := newFakeSwitch(t)
        c := newESLClient(testClientConfig(fs))
        defer c.Close()
        waitReady(t, c, 2*time.Second)

        // Synchronous api command: the result rides an api/response body.
        fr, err := c.Do(context.Background(), "api status")
        if err != nil {
                t.Fatalf("api status: %v", err)
        }
        if fr.ContentType != ctAPIResponse || fr.Result() != "+OK fake switch up" {
                t.Fatalf("api status round-trip wrong: ct=%q result=%q", fr.ContentType, fr.Result())
        }

        // bgapi: the command/reply carries the Job-UUID for correlation.
        fr, err = c.Do(context.Background(), "bgapi originate {origination_uuid=11111111-1111-1111-1111-111111111111}user/1001")
        if err != nil {
                t.Fatalf("bgapi originate: %v", err)
        }
        if fr.ContentType != ctCommandReply {
                t.Fatalf("bgapi reply content type = %q", fr.ContentType)
        }
        job := fr.Header(hdrJobUUID)
        if job == "" || fr.ReplyText() != "+OK Job-UUID: "+job {
                t.Fatalf("bgapi reply must carry Job-UUID (header %q, reply-text %q)", job, fr.ReplyText())
        }
}

func TestESLClientEventDelivery(t *testing.T) {
        fs := newFakeSwitch(t)
        cfg := testClientConfig(fs)
        events := make(chan string, 4)
        cfg.onEvent = func(name string, fields map[string]string, payload string) {
                events <- name + "|" + EventField(fields, "Unique-ID")
        }
        c := newESLClient(cfg)
        defer c.Close()
        waitReady(t, c, 2*time.Second)

        fs.injectEvent(map[string]string{
                "Event-Name":   "CHANNEL_HANGUP",
                "Unique-ID":    "11111111-1111-1111-1111-111111111111",
                "Hangup-Cause": "NORMAL_CLEARING",
        }, "")

        select {
        case got := <-events:
                if want := "CHANNEL_HANGUP|11111111-1111-1111-1111-111111111111"; got != want {
                        t.Fatalf("event delivery = %q, want %q", got, want)
                }
        case <-time.After(2 * time.Second):
                t.Fatal("injected event never reached the onEvent hook")
        }
}

func TestESLClientReconnectsAfterSwitchRestart(t *testing.T) {
        fs := newFakeSwitch(t)
        c := newESLClient(testClientConfig(fs))
        defer c.Close()
        waitReady(t, c, 2*time.Second)

        before := fs.numSubscribes()
        fs.restart() // kill listener + every session; re-open the same address

        // The subscribe only happens on a freshly established session, so its
        // count is the reconnect signal (Ready() itself can lag the kill by the
        // few ms the old session needs to notice the closed socket).
        deadline := time.Now().Add(5 * time.Second)
        for fs.numSubscribes() != before+1 {
                if time.Now().After(deadline) {
                        t.Fatalf("client did not re-subscribe after switch restart (subscribes=%d, was %d)",
                                fs.numSubscribes(), before)
                }
                time.Sleep(5 * time.Millisecond)
        }

        // The rebuilt session carries commands too.
        fr, err := c.Do(context.Background(), "api status")
        if err != nil || fr.Result() != "+OK fake switch up" {
                t.Fatalf("command after reconnect: fr=%v err=%v", fr, err)
        }
}

func TestESLClientCommandTimeoutForcesRebuild(t *testing.T) {
        fs := newFakeSwitch(t)
        fs.setReplyDelay(func(line string) time.Duration {
                if line == "api status" {
                        return 2 * time.Second // way past the client's budget
                }
                return 0
        })
        c := newESLClient(testClientConfig(fs))
        defer c.Close()
        waitReady(t, c, 2*time.Second)

        ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
        _, err := c.Do(ctx, "api status")
        cancel()
        if err == nil {
                t.Fatal("command past its deadline must fail")
        }
        if !errors.Is(err, context.DeadlineExceeded) {
                t.Fatalf("timeout must surface as a deadline error, got: %v", err)
        }

        // api replies carry no correlation id, so the timed-out command's
        // connection was rebuilt; the next command must run on a fresh,
        // responsive session.
        fs.setReplyDelay(nil)
        waitReady(t, c, 5*time.Second)
        fr, err := c.Do(context.Background(), "api status")
        if err != nil || fr.Result() != "+OK fake switch up" {
                t.Fatalf("command after timeout rebuild: fr=%v err=%v", fr, err)
        }
}
