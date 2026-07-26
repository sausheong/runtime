package browser

import (
	"testing"

	"github.com/chromedp/cdproto/fetch"
)

func TestProxyAuthResponseNeverLeaksCredentialToOrigin(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	proxy := proxyAuthResponse(&fetch.AuthChallenge{Source: fetch.AuthChallengeSourceProxy}, token)
	if proxy.Response != fetch.AuthChallengeResponseResponseProvideCredentials ||
		proxy.Username != "runtime" || proxy.Password != token {
		t.Fatalf("proxy challenge response = %#v", proxy)
	}
	for _, challenge := range []*fetch.AuthChallenge{
		nil,
		{Source: fetch.AuthChallengeSourceServer},
	} {
		response := proxyAuthResponse(challenge, token)
		if response.Response != fetch.AuthChallengeResponseResponseCancelAuth {
			t.Fatalf("non-proxy challenge response = %#v", response)
		}
		if response.Username != "" || response.Password != "" {
			t.Fatal("runtime proxy credential supplied to an origin challenge")
		}
	}
}
