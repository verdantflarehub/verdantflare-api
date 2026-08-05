package service

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

type fixedSD2MediaResolver struct {
	addresses map[string][]net.IPAddr
	err       error
}

func (r fixedSD2MediaResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.addresses[host], nil
}

func parseSD2TestRequest(t *testing.T, raw string) map[string]any {
	t.Helper()
	t.Setenv(SD2RequestIdentityHMACKeyEnv, "0123456789abcdef0123456789abcdef")
	var request map[string]any
	require.NoError(t, common.Unmarshal([]byte(raw), &request))
	return request
}

func TestNormalizeSD2CreateRequestPreservesPublicSemantics(t *testing.T) {
	request := parseSD2TestRequest(t, `{
		"model":"verdantflare-sd2",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"A bird"},
			{"type":"video_url","video_url":{"url":"https://media.example/ref-1.mp4?a=1"}},
			{"type":"image_url","image_url":{"url":"https://media.example/ref.jpg"}}
		]}],
		"duration":4,
		"ratio":"16:9",
		"generate_audio":false,
		"watermark":false,
		"resolution":"720p"
	}`)
	normalized, err := NormalizeSD2CreateRequest(request, "generate")
	require.NoError(t, err)
	require.Len(t, normalized.RequestDigest, 64)
	require.Equal(t, SD2RequestDigestVersion, normalized.DigestVersion)
	require.Equal(t, 1, normalized.VideoCount)
	require.Equal(t, 1, normalized.ImageCount)
	require.Equal(t, 0, normalized.AudioCount)
	require.False(t, normalized.GenerateAudio)
	require.Contains(t, normalized.RequestSummary, `"generate_audio":false`)
	require.NotContains(t, string(normalized.CanonicalJSON), "A bird")
	require.NotContains(t, string(normalized.CanonicalJSON), "media.example")
}

func TestNormalizeSD2CreateRequestCanonicalizesUnicodeNFC(t *testing.T) {
	composed := parseSD2TestRequest(t, `{"model":"verdantflare-sd2","messages":[{"role":"user","content":[{"type":"text","text":"café"}]}]}`)
	decomposed := parseSD2TestRequest(t, `{"model":"verdantflare-sd2","messages":[{"role":"user","content":[{"type":"text","text":"café"}]}]}`)
	a, err := NormalizeSD2CreateRequest(composed, "generate")
	require.NoError(t, err)
	b, err := NormalizeSD2CreateRequest(decomposed, "generate")
	require.NoError(t, err)
	require.Equal(t, a.RequestDigest, b.RequestDigest)
}

func TestNormalizeSD2CreateRequestURLIdentityRules(t *testing.T) {
	previousHosts, existed := os.LookupEnv("SD2_TRUSTED_ASSET_HOSTS")
	require.NoError(t, os.Setenv("SD2_TRUSTED_ASSET_HOSTS", "cache.example"))
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("SD2_TRUSTED_ASSET_HOSTS", previousHosts)
		} else {
			_ = os.Unsetenv("SD2_TRUSTED_ASSET_HOSTS")
		}
	})

	makeRequest := func(mediaURL string) map[string]any {
		return parseSD2TestRequest(t, `{"model":"verdantflare-sd2","messages":[{"role":"user","content":[{"type":"text","text":"p"},{"type":"image_url","image_url":{"url":"`+mediaURL+`"}}]}]}`)
	}
	trustedA, err := NormalizeSD2CreateRequest(makeRequest("https://cache.example/a.jpg?signature=one"), "generate")
	require.NoError(t, err)
	trustedB, err := NormalizeSD2CreateRequest(makeRequest("https://cache.example/a.jpg?signature=two"), "generate")
	require.NoError(t, err)
	require.NotEqual(t, trustedA.RequestDigest, trustedB.RequestDigest)

	externalA, err := NormalizeSD2CreateRequest(makeRequest("https://outside.example/a.jpg?signature=one"), "generate")
	require.NoError(t, err)
	externalB, err := NormalizeSD2CreateRequest(makeRequest("https://outside.example/a.jpg?signature=two"), "generate")
	require.NoError(t, err)
	require.NotEqual(t, externalA.RequestDigest, externalB.RequestDigest)
}

func TestNormalizeSD2CreateRequestRejectsUnsupportedResolutionAndMediaFirst(t *testing.T) {
	resolution := parseSD2TestRequest(t, `{"model":"verdantflare-sd2","resolution":"1080P","prompt":"p"}`)
	_, err := NormalizeSD2CreateRequest(resolution, "generate")
	var requestErr *SD2CreateRequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, "unsupported_resolution", requestErr.Code)

	mediaFirst := parseSD2TestRequest(t, `{"model":"verdantflare-sd2","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.jpg"}},{"type":"text","text":"p"}]}]}`)
	_, err = NormalizeSD2CreateRequest(mediaFirst, "generate")
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, "missing_prompt", requestErr.Code)
}

func TestNormalizeSD2CreateRequestIncludesLegacyMediaAndMetadataOptions(t *testing.T) {
	base := parseSD2TestRequest(t, `{
		"model":"verdantflare-sd2",
		"prompt":"legacy prompt",
		"image":"https://media.example/a.jpg",
		"videos":["https://media.example/v1.mp4"],
		"metadata":{"duration":"4","ratio":"9:16","generate_audio":false,"watermark":false}
	}`)
	a, err := NormalizeSD2CreateRequest(base, "generate")
	require.NoError(t, err)
	require.Equal(t, 4, a.Duration)
	require.Equal(t, "9:16", a.Ratio)
	require.False(t, a.GenerateAudio)
	require.Equal(t, 1, a.ImageCount)
	require.Equal(t, 1, a.VideoCount)

	changed := parseSD2TestRequest(t, `{
		"model":"verdantflare-sd2",
		"prompt":"legacy prompt",
		"image":"https://media.example/b.jpg",
		"videos":["https://media.example/v1.mp4"],
		"metadata":{"duration":"4","ratio":"9:16","generate_audio":false,"watermark":false}
	}`)
	b, err := NormalizeSD2CreateRequest(changed, "generate")
	require.NoError(t, err)
	require.NotEqual(t, a.RequestDigest, b.RequestDigest)
}

func TestNormalizeSD2CreateRequestRejectsConflictingContentAndProviderFields(t *testing.T) {
	conflicting := parseSD2TestRequest(t, `{
		"model":"verdantflare-sd2",
		"messages":[{"role":"user","content":[{"type":"text","text":"p"}]}],
		"metadata":{"content":[{"type":"text","text":"other"}]}
	}`)
	_, err := NormalizeSD2CreateRequest(conflicting, "generate")
	var requestErr *SD2CreateRequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, "conflicting_content", requestErr.Code)

	controlled := parseSD2TestRequest(t, `{"model":"verdantflare-sd2","prompt":"p","return_last_frame":false}`)
	_, err = NormalizeSD2CreateRequest(controlled, "generate")
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, "unsupported_provider_field", requestErr.Code)
}

func TestNormalizeSD2CreateRequestRejectsFieldsOutsidePublicAllowlists(t *testing.T) {
	for _, testCase := range []struct {
		name string
		raw  string
		code string
	}{
		{
			name: "root",
			raw:  `{"model":"verdantflare-sd2","prompt":"p","z_private":true,"a_private":true}`,
			code: "unsupported_request_field",
		},
		{
			name: "metadata",
			raw:  `{"model":"verdantflare-sd2","prompt":"p","metadata":{"provider_option":true}}`,
			code: "unsupported_metadata_field",
		},
		{
			name: "message",
			raw:  `{"model":"verdantflare-sd2","messages":[{"role":"user","content":"p","name":"hidden"}]}`,
			code: "unsupported_message_field",
		},
		{
			name: "content item",
			raw:  `{"model":"verdantflare-sd2","messages":[{"role":"user","content":[{"type":"text","text":"p","provider_hint":true}]}]}`,
			code: "unsupported_content_field",
		},
		{
			name: "media object",
			raw:  `{"model":"verdantflare-sd2","messages":[{"role":"user","content":[{"type":"text","text":"p"},{"type":"image_url","image_url":{"url":"https://assets.example/a.png","headers":{"x":"y"}}}]}]}`,
			code: "unsupported_content_field",
		},
		{
			name: "provider field in metadata",
			raw:  `{"model":"verdantflare-sd2","prompt":"p","metadata":{"channel_id":19}}`,
			code: "unsupported_provider_field",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := parseSD2TestRequest(t, testCase.raw)
			_, err := NormalizeSD2CreateRequest(request, "generate")
			var requestErr *SD2CreateRequestError
			require.ErrorAs(t, err, &requestErr)
			require.Equal(t, testCase.code, requestErr.Code)
			if testCase.name == "root" {
				require.Contains(t, requestErr.Message, "a_private")
			}
		})
	}
}

func TestNormalizeSD2CreateRequestUsesDedicatedStableIdentityKey(t *testing.T) {
	request := parseSD2TestRequest(t, `{"model":"verdantflare-sd2","prompt":"p","image":"https://assets.example/a.png?sig=one"}`)
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "application-secret-one"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	first, err := NormalizeSD2CreateRequest(request, "generate")
	require.NoError(t, err)
	common.CryptoSecret = "application-secret-two"
	second, err := NormalizeSD2CreateRequest(request, "generate")
	require.NoError(t, err)
	require.Equal(t, first.RequestDigest, second.RequestDigest)

	t.Setenv(SD2RequestIdentityHMACKeyEnv, "fedcba9876543210fedcba9876543210")
	third, err := NormalizeSD2CreateRequest(request, "generate")
	require.NoError(t, err)
	require.NotEqual(t, first.RequestDigest, third.RequestDigest)
}

func TestSD2ChannelCredentialFingerprintUsesDedicatedStableIdentityKey(t *testing.T) {
	t.Setenv(SD2RequestIdentityHMACKeyEnv, "0123456789abcdef0123456789abcdef")
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "application-secret-one"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	first := SD2ChannelCredentialFingerprint("provider-key")
	require.Len(t, first, 64)
	common.CryptoSecret = "application-secret-two"
	require.Equal(t, first, SD2ChannelCredentialFingerprint("provider-key"))

	t.Setenv(SD2RequestIdentityHMACKeyEnv, "fedcba9876543210fedcba9876543210")
	require.NotEqual(t, first, SD2ChannelCredentialFingerprint("provider-key"))

	t.Setenv(SD2RequestIdentityHMACKeyEnv, "")
	require.Empty(t, SD2ChannelCredentialFingerprint("provider-key"))
}

func TestNormalizeSD2CreateRequestRejectsConflictingDurationAliases(t *testing.T) {
	conflicting := parseSD2TestRequest(t, `{
		"model":"verdantflare-sd2",
		"prompt":"p",
		"duration":10,
		"metadata":{"seconds":"5"}
	}`)
	_, err := NormalizeSD2CreateRequest(conflicting, "generate")
	var requestErr *SD2CreateRequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, "conflicting_duration", requestErr.Code)

	equal := parseSD2TestRequest(t, `{
		"model":"verdantflare-sd2",
		"prompt":"p",
		"duration":10,
		"seconds":"10",
		"metadata":{"duration":"10","seconds":10}
	}`)
	normalized, err := NormalizeSD2CreateRequest(equal, "generate")
	require.NoError(t, err)
	require.Equal(t, 10, normalized.Duration)
}

func TestSD2SubmissionLedgerEnabledDefaultsClosed(t *testing.T) {
	t.Setenv(SD2SubmissionLedgerEnv, "")
	require.False(t, SD2SubmissionLedgerEnabled())

	t.Setenv(SD2SubmissionLedgerEnv, "not-a-boolean")
	require.False(t, SD2SubmissionLedgerEnabled())

	t.Setenv(SD2SubmissionLedgerEnv, "true")
	require.True(t, SD2SubmissionLedgerEnabled())
}

func TestConfigureSD2SubmissionIdentityFromEnvFailsClosed(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		enabled string
		batch   string
		key     string
		wantErr bool
	}{
		{name: "disabled and absent", enabled: "false"},
		{name: "enabled and absent", enabled: "true", wantErr: true},
		{name: "enabled and short", enabled: "true", key: "too-short", wantErr: true},
		{name: "disabled but malformed key", enabled: "false", key: "too-short", wantErr: true},
		{name: "malformed gate", enabled: "truthy", key: "0123456789abcdef0123456789abcdef", wantErr: true},
		{name: "enabled with batch updates", enabled: "true", batch: "true", key: "0123456789abcdef0123456789abcdef", wantErr: true},
		{name: "enabled with malformed batch gate", enabled: "true", batch: "truthy", key: "0123456789abcdef0123456789abcdef", wantErr: true},
		{name: "enabled with batch updates explicitly disabled", enabled: "true", batch: "false", key: "0123456789abcdef0123456789abcdef"},
		{name: "enabled and stable", enabled: "true", key: "0123456789abcdef0123456789abcdef"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(SD2SubmissionLedgerEnv, testCase.enabled)
			t.Setenv(SD2BatchUpdateEnv, testCase.batch)
			t.Setenv(SD2RequestIdentityHMACKeyEnv, testCase.key)
			err := ConfigureSD2SubmissionIdentityFromEnv()
			if testCase.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateSD2CreateMediaAddressesIsMandatoryAndPublicOnly(t *testing.T) {
	requestFor := func(mediaURL string) map[string]any {
		return parseSD2TestRequest(t, `{"model":"verdantflare-sd2","messages":[{"role":"user","content":[{"type":"text","text":"p"},{"type":"image_url","image_url":{"url":"`+mediaURL+`"}}]}]}`)
	}

	publicResolver := fixedSD2MediaResolver{addresses: map[string][]net.IPAddr{
		"assets.example": {{IP: net.ParseIP("93.184.216.34")}},
	}}
	require.NoError(t, validateSD2CreateMediaAddresses(context.Background(), requestFor("https://assets.example/input.png"), publicResolver))

	privateResolver := fixedSD2MediaResolver{addresses: map[string][]net.IPAddr{
		"assets.example": {{IP: net.ParseIP("10.0.0.8")}},
	}}
	for _, testCase := range []struct {
		name     string
		mediaURL string
		resolver sd2MediaResolver
	}{
		{name: "private literal", mediaURL: "https://127.0.0.1/input.png", resolver: publicResolver},
		{name: "private dns", mediaURL: "https://assets.example/input.png", resolver: privateResolver},
		{name: "non tls port", mediaURL: "https://assets.example:8443/input.png", resolver: publicResolver},
		{name: "dns failure", mediaURL: "https://assets.example/input.png", resolver: fixedSD2MediaResolver{err: errors.New("dns unavailable")}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateSD2CreateMediaAddresses(context.Background(), requestFor(testCase.mediaURL), testCase.resolver)
			var requestErr *SD2CreateRequestError
			require.ErrorAs(t, err, &requestErr)
			require.Equal(t, "invalid_media_url", requestErr.Code)
		})
	}
}
