package mojang

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New()
	c.profileURL = srv.URL + "/profile/"
	c.nameURL = srv.URL + "/name/"
	return c
}

func TestNameFor(t *testing.T) {
	var got string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.Write([]byte(`{"id":"069a79f444e94726a5befca90e38aaf5","name":"NotchNew"}`))
	})

	name, ok := c.NameFor("069a79f4-44e9-4726-a5be-fca90e38aaf5")
	if !ok || name != "NotchNew" {
		t.Fatalf("NameFor = %q, %v", name, ok)
	}
	// The API takes bare hex, so the dashes have to come off.
	if got != "/profile/069a79f444e94726a5befca90e38aaf5" {
		t.Fatalf("requested %q", got)
	}
}

func TestUUIDFor(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"069a79f444e94726a5befca90e38aaf5","name":"Notch"}`))
	})
	uuid, ok := c.UUIDFor("Notch", "198.51.100.7")
	if !ok || uuid != "069a79f444e94726a5befca90e38aaf5" {
		t.Fatalf("UUIDFor = %q, %v", uuid, ok)
	}
}

// An unknown name answers 204 or 404, and must read as "nobody", not as an error
// that might be retried into a lookup storm.
func TestUnknownNameIsNotAnError(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotFound} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		})
		if _, ok := c.UUIDFor("Nobody", "198.51.100.7"); ok {
			t.Errorf("status %d reported a result", code)
		}
	}
}

func TestServerErrorIsNotAResult(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if _, ok := c.UUIDFor("Notch", "198.51.100.7"); ok {
		t.Fatal("a 429 was read as a result")
	}
}

func TestGarbageBodyIsNotAResult(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not json</html>"))
	})
	if _, ok := c.UUIDFor("Notch", "198.51.100.7"); ok {
		t.Fatal("a non-JSON body was read as a result")
	}
}

// The name comes off the wire from an unauthenticated client, so it must not be
// able to steer the request path.
func TestNameIsEscaped(t *testing.T) {
	var got string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNotFound)
	})
	c.UUIDFor("../../burgle", "198.51.100.7")
	if strings.Contains(got, "../") {
		t.Fatalf("path traversal reached the request: %q", got)
	}
}

// The limiters gate the login-side lookup; the refresh walk is ours and is not
// subject to them.
func TestUUIDForIsGatedAndNameForIsNot(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"id":"069a79f444e94726a5befca90e38aaf5","name":"Notch"}`))
	})

	for i := 0; i < 6; i++ {
		c.UUIDFor("Notch", "198.51.100.7")
	}
	if calls != 3 {
		t.Fatalf("%d login-side lookups reached the network, want 3", calls)
	}

	calls = 0
	for i := 0; i < 6; i++ {
		c.NameFor("069a79f4-44e9-4726-a5be-fca90e38aaf5")
	}
	if calls != 6 {
		t.Fatalf("%d refresh lookups reached the network, want 6", calls)
	}
}

// The control link adds players by name, and the reply to a member differs
// between a name nobody holds and Mojang not answering.
func TestLookupNameTellsUnknownFromUnreachable(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"069a79f444e94726a5befca90e38aaf5","name":"Notch"}`))
	})
	uuid, name, err := c.LookupName("notch")
	if err != nil || uuid != "069a79f444e94726a5befca90e38aaf5" || name != "Notch" {
		t.Fatalf("LookupName = %q, %q, %v", uuid, name, err)
	}

	c = testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, _, err := c.LookupName("Nobody"); !errors.Is(err, ErrNoSuchPlayer) {
		t.Fatalf("unknown name: %v", err)
	}

	c = testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	if _, _, err := c.LookupName("Notch"); err == nil || errors.Is(err, ErrNoSuchPlayer) {
		t.Fatalf("unreachable read as an answer: %v", err)
	}
}

func TestLookupUUIDIsNotGated(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"id":"069a79f444e94726a5befca90e38aaf5","name":"Notch"}`))
	})
	for i := 0; i < 6; i++ {
		if name, err := c.LookupUUID("069a79f4-44e9-4726-a5be-fca90e38aaf5"); err != nil || name != "Notch" {
			t.Fatalf("LookupUUID = %q, %v", name, err)
		}
	}
	if calls != 6 {
		t.Fatalf("%d lookups reached the network, want 6", calls)
	}
}
