package factory

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	atmsg "github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/africastalking"
	twiliomsg "github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/twilio"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/whatsappcloud"
	atvoice "github.com/Roy-Wanyoike/orvexa/internal/telephony/adapters/africastalking"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony/adapters/asterisk"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony/adapters/freeswitch"
	twiliovoice "github.com/Roy-Wanyoike/orvexa/internal/telephony/adapters/twilio"
)

// Fake fixture values — clearly fake, never real credential material. They
// are pointed only at loopback fakes and must never appear in any error
// message (asserted below).
const (
	fakeSID    = "ACFAKE0000000000000000000001"
	fakeToken  = "fake-token-00000000abcd"
	fakeNumber = "+15550000001"
	fakeATUser = "fake-at-user"
	fakeATKey  = "fake-at-key-000000abcd"
	fakeSender = "FAKEID"
	fakePhone  = "100000000000000"
)

// allProviderEnvVars lists every environment variable the registry reads so
// tests run hermetically: a host shell carrying real ORVEXA_* credentials
// must not influence these results.
var allProviderEnvVars = []string{
	"ORVEXA_TELEPHONY_PROVIDER", "ORVEXA_MESSAGING_PROVIDER",
	"ORVEXA_TWILIO_ACCOUNT_SID", "ORVEXA_TWILIO_AUTH_TOKEN", "ORVEXA_TWILIO_FROM_NUMBER",
	"ORVEXA_WHATSAPP_PHONE_NUMBER_ID", "ORVEXA_WHATSAPP_ACCESS_TOKEN",
	"ORVEXA_WHATSAPP_APP_SECRET", "ORVEXA_WHATSAPP_VERIFY_TOKEN",
	"ORVEXA_AT_USERNAME", "ORVEXA_AT_API_KEY", "ORVEXA_AT_VOICE_PRODUCT_CODE", "ORVEXA_AT_SENDER_ID",
	"ORVEXA_FREESWITCH_HOST", "ORVEXA_FREESWITCH_PORT", "ORVEXA_FREESWITCH_PASSWORD",
	"ORVEXA_ASTERISK_HOST", "ORVEXA_ASTERISK_PORT", "ORVEXA_ASTERISK_USERNAME", "ORVEXA_ASTERISK_SECRET",
}

// hermeticEnv pins every provider env var to empty so the outer environment
// (a configured deployment shell, CI secrets) cannot leak into assertions.
func hermeticEnv(t *testing.T) {
	t.Helper()
	for _, k := range allProviderEnvVars {
		t.Setenv(k, "")
	}
}

// countingIngest stands in for the platform ingest port. These tests assert
// construction and wiring shape, not lifecycle traffic — but the adapters
// require the hook to be present, correctly: an adapter without a delivery
// path could never report its lifecycle through the fail-closed gateway.
func countingIngest() comms.IngestFunc {
	return func(context.Context, string, []byte, string) error { return nil }
}

func testSigner() comms.Signer {
	return func(body []byte) string { return "test-sig" }
}

// deadPBX is a loopback TCP listener for the session-based adapters
// (FreeSWITCH ESL, Asterisk AMI): it accepts and immediately closes, so the
// adapters' background supervisors exercise their real dial path against
// the factory-mapped endpoint without any external network.
type deadPBX struct {
	ln net.Listener

	mu    sync.Mutex
	conns int
}

func newDeadPBX(t *testing.T) *deadPBX {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	p := &deadPBX{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns++
			p.mu.Unlock()
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *deadPBX) hostPort() (string, string) {
	host, port, _ := net.SplitHostPort(p.ln.Addr().String())
	return host, port
}

func (p *deadPBX) touches() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns
}

func waitFor(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// typeName pins the concrete adapter type Build returns for a provider.
func typeName(v any) string { return fmt.Sprintf("%T", v) }

// TestBuildVoiceProviderTable drives the voice-plane half of Build across
// every registered provider name and asserts the correct, non-nil adapter
// type comes back — constructed against loopback fakes only, never the real
// carriers. Session-based adapters additionally prove their dial-style
// constructors received the factory-mapped endpoint.
func TestBuildVoiceProviderTable(t *testing.T) {
	hermeticEnv(t)

	// Loopback stand-ins for the deployment endpoints the factory injects
	// (Twilio TwiML/callback documents); construction performs zero HTTP,
	// so the fake never answers a request.
	endpoints := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(endpoints.Close)

	pbx := newDeadPBX(t)
	pbxHost, pbxPort := pbx.hostPort()

	cases := []struct {
		name string
		tel  *registry.TelephonyConfig
		want string
		dial bool // session-based adapter: must dial the fake PBX
	}{
		{
			name: "simulator",
			tel:  &registry.TelephonyConfig{Provider: registry.ProviderSimulator},
			want: typeName(&comms.Simulator{}),
		},
		{
			name: "twilio",
			tel: &registry.TelephonyConfig{
				Provider:         registry.ProviderTwilio,
				TwilioAccountSID: fakeSID,
				TwilioAuthToken:  fakeToken,
				TwilioFromNumber: fakeNumber,
			},
			want: typeName(&twiliovoice.Adapter{}),
		},
		{
			name: "africastalking",
			tel: &registry.TelephonyConfig{
				Provider:           registry.ProviderAfricasTalking,
				ATUsername:         fakeATUser,
				ATAPIKey:           fakeATKey,
				ATVoiceProductCode: "v01ce",
			},
			want: typeName(&atvoice.Adapter{}),
		},
		{
			name: "freeswitch",
			tel: &registry.TelephonyConfig{
				Provider:           registry.ProviderFreeSwitch,
				FreeSwitchHost:     pbxHost,
				FreeSwitchPort:     pbxPort,
				FreeSwitchPassword: fakeToken,
			},
			want: typeName(&freeswitch.Adapter{}),
			dial: true,
		},
		{
			name: "asterisk",
			tel: &registry.TelephonyConfig{
				Provider:         registry.ProviderAsterisk,
				AsteriskHost:     pbxHost,
				AsteriskPort:     pbxPort,
				AsteriskUsername: fakeATUser,
				AsteriskSecret:   fakeToken,
			},
			want: typeName(&asterisk.Voice{}),
			dial: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			voice, messenger, teardown, err := Build(context.Background(), Config{
				Telephony: tc.tel,
				Messaging: &registry.MessagingConfig{Provider: registry.ProviderSimulator},
				Ingest:    countingIngest(),
				Signer:    testSigner(),

				TwilioVoiceTwiMLBaseURL:    endpoints.URL + "/twiml",
				TwilioVoiceCallbackBaseURL: endpoints.URL + "/callback",
				TwilioVoiceHoldTwiMLURL:    endpoints.URL + "/hold",
				TwilioVoiceResumeURL:       endpoints.URL + "/resume",
			})
			if err != nil {
				t.Fatalf("Build(%s): %v", tc.name, err)
			}
			if voice == nil || messenger == nil || teardown == nil {
				t.Fatalf("Build(%s) returned nil wiring", tc.name)
			}
			if _, ok := messenger.(*comms.Simulator); !ok {
				t.Fatalf("messaging plane must stay simulator when configured so, got %T", messenger)
			}
			if got := typeName(voice); got != tc.want {
				t.Fatalf("provider %s: got %s, want %s", tc.name, got, tc.want)
			}

			if tc.dial {
				waitFor(t, 2*time.Second, tc.name+" dial against the loopback fake PBX", func() bool {
					return pbx.touches() > 0
				})
			}

			teardown() // graceful shutdown path
			teardown() // must be idempotent (adapters guard their close)
		})
	}
}

// TestBuildMessagingProviderTable does the same for the messaging plane.
func TestBuildMessagingProviderTable(t *testing.T) {
	hermeticEnv(t)

	endpoints := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(endpoints.Close)

	cases := []struct {
		name string
		msg  *registry.MessagingConfig
		want string
	}{
		{
			name: "simulator",
			msg:  &registry.MessagingConfig{Provider: registry.ProviderSimulator},
			want: typeName(&comms.Simulator{}),
		},
		{
			name: "twilio",
			msg: &registry.MessagingConfig{
				Provider:         registry.ProviderTwilio,
				TwilioAccountSID: fakeSID,
				TwilioAuthToken:  fakeToken,
				TwilioFromNumber: fakeNumber,
			},
			want: typeName(&twiliomsg.Adapter{}),
		},
		{
			name: "whatsappcloud",
			msg: &registry.MessagingConfig{
				Provider:              registry.ProviderWhatsAppCloud,
				WhatsAppPhoneNumberID: fakePhone,
				WhatsAppAccessToken:   fakeToken,
				WhatsAppAppSecret:     fakeToken,
				WhatsAppVerifyToken:   fakeToken,
			},
			want: typeName(&whatsappcloud.Provider{}),
		},
		{
			name: "africastalking",
			msg: &registry.MessagingConfig{
				Provider:   registry.ProviderAfricasTalking,
				ATUsername: fakeATUser,
				ATAPIKey:   fakeATKey,
				ATSenderID: fakeSender,
			},
			want: typeName(&atmsg.Adapter{}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			voice, messenger, teardown, err := Build(context.Background(), Config{
				Telephony: &registry.TelephonyConfig{Provider: registry.ProviderSimulator},
				Messaging: tc.msg,
				Ingest:    countingIngest(),
				Signer:    testSigner(),

				TwilioMsgStatusCallbackBase: endpoints.URL + "/status",
			})
			if err != nil {
				t.Fatalf("Build(%s): %v", tc.name, err)
			}
			if _, ok := voice.(*comms.Simulator); !ok {
				t.Fatalf("telephony plane must stay simulator when configured so, got %T", voice)
			}
			if got := typeName(messenger); got != tc.want {
				t.Fatalf("provider %s: got %s, want %s", tc.name, got, tc.want)
			}
			teardown()
			teardown()
		})
	}
}

// TestBuildUnsetEnvSelectsSimulator pins the honesty contract end-to-end:
// with both provider env vars unset, registry.LoadFromEnv resolves the
// simulator, Build wires it for both planes as one instance, and
// LogSelection prints the explicit provider=simulator reason=not_configured
// line.
func TestBuildUnsetEnvSelectsSimulator(t *testing.T) {
	hermeticEnv(t)

	tel, msg, err := registry.LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv with unset providers: %v", err)
	}
	if tel.Provider != registry.ProviderSimulator || msg.Provider != registry.ProviderSimulator {
		t.Fatalf("unset env must resolve provider=simulator, got telephony=%s messaging=%s", tel.Provider, msg.Provider)
	}

	voice, messenger, teardown, err := Build(context.Background(), Config{
		Telephony: tel, Messaging: msg,
		Ingest: countingIngest(), Signer: testSigner(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	sv, ok := voice.(*comms.Simulator)
	if !ok {
		t.Fatalf("unset env must wire the simulator voice plane, got %T", voice)
	}
	sm, ok := messenger.(*comms.Simulator)
	if !ok {
		t.Fatalf("unset env must wire the simulator messaging plane, got %T", messenger)
	}
	if sv != sm {
		t.Fatal("both planes on the simulator must share one instance (pre-factory wiring parity)")
	}
	teardown()
	teardown()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	LogSelection(log, "telephony", string(tel.Provider))
	LogSelection(log, "messaging", string(msg.Provider))
	out := buf.String()
	for _, want := range []string{"provider=simulator", "reason=not_configured", "plane=telephony", "plane=messaging"} {
		if !strings.Contains(out, want) {
			t.Fatalf("honest startup log missing %q; got:\n%s", want, out)
		}
	}
	if strings.Count(out, "reason=not_configured") != 2 {
		t.Fatalf("both simulator planes must assert reason=not_configured; got:\n%s", out)
	}
}

// TestBuildEnvToAdapters drives the full production path — env vars →
// registry.LoadFromEnv → Build — proving the credential fields land in the
// right constructors (fakes at loopback only).
func TestBuildEnvToAdapters(t *testing.T) {
	hermeticEnv(t)
	t.Setenv("ORVEXA_TELEPHONY_PROVIDER", "asterisk")
	t.Setenv("ORVEXA_ASTERISK_HOST", "127.0.0.1") // port left unset → registry applies the 5038 default
	t.Setenv("ORVEXA_ASTERISK_USERNAME", fakeATUser)
	t.Setenv("ORVEXA_ASTERISK_SECRET", fakeToken)
	t.Setenv("ORVEXA_MESSAGING_PROVIDER", "africastalking")
	t.Setenv("ORVEXA_AT_USERNAME", fakeATUser)
	t.Setenv("ORVEXA_AT_API_KEY", fakeATKey)
	t.Setenv("ORVEXA_AT_SENDER_ID", fakeSender)

	tel, msg, err := registry.LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	voice, messenger, teardown, err := Build(context.Background(), Config{
		Telephony: tel, Messaging: msg,
		Ingest: countingIngest(), Signer: testSigner(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := voice.(*asterisk.Voice); !ok {
		t.Fatalf("ORVEXA_TELEPHONY_PROVIDER=asterisk must construct the AMI adapter, got %T", voice)
	}
	if _, ok := messenger.(*atmsg.Adapter); !ok {
		t.Fatalf("ORVEXA_MESSAGING_PROVIDER=africastalking must construct the AT adapter, got %T", messenger)
	}
	teardown()
}

// TestBuildMisconfigurationNamesEnvVar asserts the startup contract: every
// misconfiguration fails loudly and the error message names the offending
// environment VARIABLES (never their values), so an operator can act
// without a code dive.
func TestBuildMisconfigurationNamesEnvVar(t *testing.T) {
	hermeticEnv(t)

	cases := []struct {
		name      string
		setEnv    map[string]string
		wantSub   []string
		forbidden []string // credential material that must never leak
	}{
		{
			name:    "twilio selected, credentials missing",
			setEnv:  map[string]string{"ORVEXA_TELEPHONY_PROVIDER": "twilio"},
			wantSub: []string{"ORVEXA_TWILIO_ACCOUNT_SID", "ORVEXA_TWILIO_AUTH_TOKEN"},
		},
		{
			name:    "whatsappcloud selected, credentials missing",
			setEnv:  map[string]string{"ORVEXA_MESSAGING_PROVIDER": "whatsappcloud"},
			wantSub: []string{"ORVEXA_WHATSAPP_PHONE_NUMBER_ID", "ORVEXA_WHATSAPP_ACCESS_TOKEN"},
		},
		{
			name:    "africastalking voice selected, credentials missing",
			setEnv:  map[string]string{"ORVEXA_TELEPHONY_PROVIDER": "africastalking"},
			wantSub: []string{"ORVEXA_AT_USERNAME", "ORVEXA_AT_API_KEY", "ORVEXA_AT_VOICE_PRODUCT_CODE"},
		},
		{
			name: "africastalking messaging selected, sender id missing",
			setEnv: map[string]string{
				"ORVEXA_MESSAGING_PROVIDER": "africastalking",
				"ORVEXA_AT_USERNAME":        fakeATUser,
				"ORVEXA_AT_API_KEY":         fakeATKey,
			},
			wantSub:   []string{"ORVEXA_AT_SENDER_ID"},
			forbidden: []string{fakeATKey},
		},
		{
			name: "freeswitch selected, password missing",
			setEnv: map[string]string{
				"ORVEXA_TELEPHONY_PROVIDER": "freeswitch",
				"ORVEXA_FREESWITCH_HOST":    "127.0.0.1",
			},
			wantSub: []string{"ORVEXA_FREESWITCH_PASSWORD"},
		},
		{
			name: "asterisk selected, secret missing",
			setEnv: map[string]string{
				"ORVEXA_TELEPHONY_PROVIDER": "asterisk",
				"ORVEXA_ASTERISK_HOST":      "127.0.0.1",
				"ORVEXA_ASTERISK_USERNAME":  fakeATUser,
			},
			wantSub: []string{"ORVEXA_ASTERISK_SECRET"},
		},
		{
			name:    "unknown provider name",
			setEnv:  map[string]string{"ORVEXA_TELEPHONY_PROVIDER": "pigeon"},
			wantSub: []string{"unknown telephony provider", "pigeon", "freeswitch"},
		},
		{
			name:    "provider does not serve the plane",
			setEnv:  map[string]string{"ORVEXA_TELEPHONY_PROVIDER": "whatsappcloud"},
			wantSub: []string{"whatsappcloud", "voice"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.setEnv {
				t.Setenv(k, v)
			}
			_, _, err := registry.LoadFromEnv()
			if err == nil {
				t.Fatal("expected a startup error")
			}
			assertActionable(t, err.Error(), tc.wantSub, tc.forbidden)
		})
	}
}

// TestBuildDefendsValidatedContract proves Build itself fails closed even
// when handed hand-built (non-env) configurations, and that the delivery
// wiring is mandatory — an adapter without Ingest/Signer could never report
// its lifecycle through the fail-closed gateway.
func TestBuildDefendsValidatedContract(t *testing.T) {
	t.Run("unvalidated config rejected with named env var", func(t *testing.T) {
		_, _, _, err := Build(context.Background(), Config{
			Telephony: &registry.TelephonyConfig{Provider: registry.ProviderTwilio},
			Ingest:    countingIngest(),
			Signer:    testSigner(),
		})
		if err == nil {
			t.Fatal("missing twilio credentials must fail Build")
		}
		assertActionable(t, err.Error(), []string{"ORVEXA_TWILIO_ACCOUNT_SID"}, nil)
	})
	t.Run("plane mismatch rejected", func(t *testing.T) {
		_, _, _, err := Build(context.Background(), Config{
			Messaging: &registry.MessagingConfig{Provider: registry.ProviderFreeSwitch},
			Ingest:    countingIngest(),
			Signer:    testSigner(),
		})
		if err == nil {
			t.Fatal("freeswitch must not satisfy the messaging plane")
		}
		assertActionable(t, err.Error(), []string{"freeswitch", "messaging"}, nil)
	})
	t.Run("nil configs fall back to the simulator", func(t *testing.T) {
		voice, messenger, teardown, err := Build(context.Background(), Config{
			Ingest: countingIngest(), Signer: testSigner(),
		})
		if err != nil {
			t.Fatalf("nil configs must select the simulator: %v", err)
		}
		if _, ok := voice.(*comms.Simulator); !ok {
			t.Fatalf("nil telephony config must select the simulator, got %T", voice)
		}
		if _, ok := messenger.(*comms.Simulator); !ok {
			t.Fatalf("nil messaging config must select the simulator, got %T", messenger)
		}
		teardown()
	})
	t.Run("unwired delivery is a startup error", func(t *testing.T) {
		_, _, _, err := Build(context.Background(), Config{Signer: testSigner()})
		if err == nil {
			t.Fatal("nil Ingest must fail Build")
		}
		assertActionable(t, err.Error(), []string{"Ingest", "Signer"}, nil)
	})
	t.Run("nil signer is a startup error", func(t *testing.T) {
		_, _, _, err := Build(context.Background(), Config{Ingest: countingIngest()})
		if err == nil {
			t.Fatal("nil Signer must fail Build")
		}
	})
}

// TestLogSelectionRealProvider keeps the honest-log contract symmetric: a
// real provider logs provider=<name> and never the simulator reason.
func TestLogSelectionRealProvider(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	LogSelection(log, "telephony", "asterisk")
	out := buf.String()
	if !strings.Contains(out, "provider=asterisk") || !strings.Contains(out, "plane=telephony") {
		t.Fatalf("real provider must log its name; got:\n%s", out)
	}
	if strings.Contains(out, "reason=not_configured") {
		t.Fatalf("a configured real provider must not claim reason=not_configured; got:\n%s", out)
	}
	LogSelection(nil, "telephony", "twilio") // nil logger must not panic
}

// assertActionable checks the error message names what an operator needs —
// and nothing it must not see.
func assertActionable(t *testing.T, got string, wantSub, forbidden []string) {
	t.Helper()
	for _, w := range wantSub {
		if !strings.Contains(got, w) {
			t.Fatalf("error must name %q; got: %s", w, got)
		}
	}
	for _, f := range forbidden {
		if strings.Contains(got, f) {
			t.Fatalf("error must not contain credential material %q; got: %s", f, got)
		}
	}
}
