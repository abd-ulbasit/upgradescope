package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A URL (and a signing key) given as functions are asked at each delivery,
// so a rotated Secret is used by the next message with no restart.
func TestNotifiersFollowTheirURLAndSecretSources(t *testing.T) {
	var gotPath, gotSig, gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath.Store(r.URL.Path)
		gotSig.Store(r.Header.Get(SignatureHeader))
		gotBody.Store(b)
	}))
	defer srv.Close()

	url, key := srv.URL+"/first", "key-one"
	hook := NewGenericWebhook("")
	hook.URLFunc = func() string { return url }
	hook.SecretFunc = func() string { return key }
	slack := NewSlack("")
	slack.URLFunc = func() string { return url }

	n := testNotification()
	for _, tc := range []struct{ path, key string }{{"/first", "key-one"}, {"/second", "key-two"}} {
		url, key = srv.URL+tc.path, tc.key
		if err := hook.Notify(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		if gotPath.Load() != tc.path {
			t.Errorf("webhook posted to %v, want %s", gotPath.Load(), tc.path)
		}
		if want := Sign(tc.key, gotBody.Load().([]byte)); gotSig.Load() != want {
			t.Errorf("webhook signature %v, want one made with %s", gotSig.Load(), tc.key)
		}
		if hook.SinkURL() != url {
			t.Errorf("SinkURL = %q, want the current URL", hook.SinkURL())
		}
		if err := slack.Notify(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		if gotPath.Load() != tc.path {
			t.Errorf("slack posted to %v, want %s", gotPath.Load(), tc.path)
		}
	}
}
