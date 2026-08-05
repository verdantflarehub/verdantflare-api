package service

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	sd2ArchiveEnvEndpoint              = "SD2_RESULT_ARCHIVE_S3_ENDPOINT"
	sd2ArchiveEnvRegion                = "SD2_RESULT_ARCHIVE_S3_REGION"
	sd2ArchiveEnvBucket                = "SD2_RESULT_ARCHIVE_S3_BUCKET"
	sd2ArchiveEnvAccessKeyID           = "SD2_RESULT_ARCHIVE_S3_ACCESS_KEY_ID"
	sd2ArchiveEnvSecretAccessKey       = "SD2_RESULT_ARCHIVE_S3_SECRET_ACCESS_KEY"
	sd2ArchiveEnvSessionToken          = "SD2_RESULT_ARCHIVE_S3_SESSION_TOKEN"
	sd2ArchiveEnvPrefix                = "SD2_RESULT_ARCHIVE_S3_PREFIX"
	sd2ArchiveEnvUsePathStyle          = "SD2_RESULT_ARCHIVE_S3_USE_PATH_STYLE"
	sd2ArchiveEnvExpectedBucketOwner   = "SD2_RESULT_ARCHIVE_S3_EXPECTED_BUCKET_OWNER"
	sd2ArchiveEnvSSE                   = "SD2_RESULT_ARCHIVE_S3_SSE"
	sd2ArchiveEnvKMSKeyID              = "SD2_RESULT_ARCHIVE_S3_KMS_KEY_ID"
	sd2ArchiveEnvIdentityHMACKey       = "SD2_RESULT_ARCHIVE_IDENTITY_HMAC_KEY"
	sd2ArchiveEnvTempDir               = "SD2_RESULT_ARCHIVE_TEMP_DIR"
	sd2ArchiveEnvPrivateBucketVerified = "SD2_RESULT_ARCHIVE_S3_PRIVATE_BUCKET_VERIFIED"
	sd2ArchiveEnvVersioningVerified    = "SD2_RESULT_ARCHIVE_S3_VERSIONING_VERIFIED"
	sd2ArchiveEnvLifecycleVerified     = "SD2_RESULT_ARCHIVE_S3_LIFECYCLE_VERIFIED"
	sd2ArchiveEnvRetentionDays         = "SD2_RESULT_ARCHIVE_RETENTION_DAYS"
	sd2ArchiveEnvHostAllowlist         = "SD2_RESULT_ARCHIVE_HOST_ALLOWLIST_JSON"
)

var sd2ArchiveEnvironmentKeys = []string{
	sd2ArchiveEnvEndpoint,
	sd2ArchiveEnvRegion,
	sd2ArchiveEnvBucket,
	sd2ArchiveEnvAccessKeyID,
	sd2ArchiveEnvSecretAccessKey,
	sd2ArchiveEnvSessionToken,
	sd2ArchiveEnvPrefix,
	sd2ArchiveEnvUsePathStyle,
	sd2ArchiveEnvExpectedBucketOwner,
	sd2ArchiveEnvSSE,
	sd2ArchiveEnvKMSKeyID,
	sd2ArchiveEnvIdentityHMACKey,
	sd2ArchiveEnvTempDir,
	sd2ArchiveEnvPrivateBucketVerified,
	sd2ArchiveEnvVersioningVerified,
	sd2ArchiveEnvLifecycleVerified,
	sd2ArchiveEnvRetentionDays,
	sd2ArchiveEnvHostAllowlist,
}

type sd2S3ArchiveConfig struct {
	Endpoint            string
	Region              string
	Bucket              string
	AccessKeyID         string
	SecretAccessKey     string
	SessionToken        string
	Prefix              string
	UsePathStyle        bool
	ExpectedBucketOwner string
	SSE                 types.ServerSideEncryption
	KMSKeyID            string
	IdentityHMACKey     []byte
	TempDir             string
	RetentionDays       int
	ResultHosts         map[string]map[int][]string
}

type sd2ArchiveEnvLookup func(string) (string, bool)

// ConfigureSD2ResultArchiveFromEnv installs the production archive and the
// exact provider/channel result-host resolver. It validates only configuration
// and the operator's explicit verification attestations: startup deliberately
// performs no bucket call and does not itself verify privacy or lifecycle
// policy or bucket versioning. Missing configuration keeps both registries nil, while partial
// configuration fails startup closed.
func ConfigureSD2ResultArchiveFromEnv() error {
	SetSD2ResultArchive(nil)
	SetSD2ResultHostAllowlistResolver(nil)

	config, err := loadSD2S3ArchiveConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	if config == nil {
		return nil
	}

	archive, err := newSD2S3ResultArchive(*config, newSD2S3Client(*config))
	if err != nil {
		return err
	}
	SetSD2ResultArchive(archive)
	SetSD2ResultHostAllowlistResolver(config.resultHostResolver())
	return nil
}

func loadSD2S3ArchiveConfig(lookup sd2ArchiveEnvLookup) (*sd2S3ArchiveConfig, error) {
	configured := false
	for _, key := range sd2ArchiveEnvironmentKeys {
		if _, ok := lookup(key); ok {
			configured = true
			break
		}
	}
	if !configured {
		return nil, nil
	}

	required := func(key string) (string, error) {
		value, ok := lookup(key)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			return "", fmt.Errorf("%s is required when the SD2 result archive is configured", key)
		}
		return value, nil
	}

	endpoint, err := required(sd2ArchiveEnvEndpoint)
	if err != nil {
		return nil, err
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme != "https" || parsedEndpoint.Hostname() == "" || parsedEndpoint.User != nil || parsedEndpoint.RawQuery != "" || parsedEndpoint.Fragment != "" || (parsedEndpoint.Path != "" && parsedEndpoint.Path != "/") {
		return nil, fmt.Errorf("%s must be an HTTPS origin", sd2ArchiveEnvEndpoint)
	}
	if portText := parsedEndpoint.Port(); portText != "" {
		port, portErr := strconv.Atoi(portText)
		if portErr != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("%s contains an invalid port", sd2ArchiveEnvEndpoint)
		}
	}
	parsedEndpoint.Path = ""
	endpoint = strings.TrimSuffix(parsedEndpoint.String(), "/")

	region, err := required(sd2ArchiveEnvRegion)
	if err != nil {
		return nil, err
	}
	if len(region) > 64 || strings.ContainsAny(region, "\x00\r\n\t /\\") {
		return nil, fmt.Errorf("%s is invalid", sd2ArchiveEnvRegion)
	}
	bucket, err := required(sd2ArchiveEnvBucket)
	if err != nil {
		return nil, err
	}
	if !validSD2ArchiveBucketName(bucket) {
		return nil, fmt.Errorf("%s is invalid", sd2ArchiveEnvBucket)
	}
	accessKeyID, err := required(sd2ArchiveEnvAccessKeyID)
	if err != nil {
		return nil, err
	}
	secretAccessKey, err := required(sd2ArchiveEnvSecretAccessKey)
	if err != nil {
		return nil, err
	}
	if len(accessKeyID) > 512 || len(secretAccessKey) > 4096 || strings.ContainsAny(accessKeyID, "\x00\r\n") || strings.ContainsAny(secretAccessKey, "\x00\r\n") {
		return nil, fmt.Errorf("SD2 result archive credentials are invalid")
	}
	sessionToken := optionalSD2ArchiveEnv(lookup, sd2ArchiveEnvSessionToken)
	if len(sessionToken) > 16384 || strings.ContainsAny(sessionToken, "\x00\r\n") {
		return nil, fmt.Errorf("%s is invalid", sd2ArchiveEnvSessionToken)
	}

	prefix := "sd2-results"
	if value, ok := lookup(sd2ArchiveEnvPrefix); ok {
		prefix = strings.Trim(strings.TrimSpace(value), "/")
	}
	if !validSD2ArchivePrefix(prefix) {
		return nil, fmt.Errorf("%s is invalid", sd2ArchiveEnvPrefix)
	}

	usePathStyle := true
	if value, ok := lookup(sd2ArchiveEnvUsePathStyle); ok {
		usePathStyle, err = strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("%s must be true or false", sd2ArchiveEnvUsePathStyle)
		}
	}

	sseText, err := required(sd2ArchiveEnvSSE)
	if err != nil {
		return nil, err
	}
	var sse types.ServerSideEncryption
	switch sseText {
	case string(types.ServerSideEncryptionAes256):
		sse = types.ServerSideEncryptionAes256
	case string(types.ServerSideEncryptionAwsKms):
		sse = types.ServerSideEncryptionAwsKms
	default:
		return nil, fmt.Errorf("%s must be AES256 or aws:kms", sd2ArchiveEnvSSE)
	}
	kmsKeyID := strings.TrimSpace(optionalSD2ArchiveEnv(lookup, sd2ArchiveEnvKMSKeyID))
	if len(kmsKeyID) > 2048 || strings.ContainsAny(kmsKeyID, "\x00\r\n") {
		return nil, fmt.Errorf("%s is invalid", sd2ArchiveEnvKMSKeyID)
	}
	if sse == types.ServerSideEncryptionAwsKms && kmsKeyID == "" {
		return nil, fmt.Errorf("%s is required for aws:kms", sd2ArchiveEnvKMSKeyID)
	}
	if sse == types.ServerSideEncryptionAwsKms && (strings.HasPrefix(kmsKeyID, "alias/") || strings.Contains(kmsKeyID, ":alias/")) {
		return nil, fmt.Errorf("%s must use a stable KMS key ID or key ARN, not an alias", sd2ArchiveEnvKMSKeyID)
	}
	if sse != types.ServerSideEncryptionAwsKms && kmsKeyID != "" {
		return nil, fmt.Errorf("%s is only valid for aws:kms", sd2ArchiveEnvKMSKeyID)
	}
	identityHMACKey, err := required(sd2ArchiveEnvIdentityHMACKey)
	if err != nil {
		return nil, err
	}
	if len(identityHMACKey) < 32 || len(identityHMACKey) > 4096 || strings.ContainsAny(identityHMACKey, "\x00\r\n") {
		return nil, fmt.Errorf("%s must be a stable secret between 32 and 4096 bytes", sd2ArchiveEnvIdentityHMACKey)
	}
	tempDir, err := required(sd2ArchiveEnvTempDir)
	if err != nil {
		return nil, err
	}
	tempDir = filepath.Clean(tempDir)
	if !filepath.IsAbs(tempDir) || isFilesystemRoot(tempDir) || strings.ContainsAny(tempDir, "\x00\r\n") {
		return nil, fmt.Errorf("%s must be a dedicated absolute directory", sd2ArchiveEnvTempDir)
	}

	if !requiredTrueSD2ArchiveEnv(lookup, sd2ArchiveEnvPrivateBucketVerified) {
		return nil, fmt.Errorf("%s must be explicitly set to true", sd2ArchiveEnvPrivateBucketVerified)
	}
	if !requiredTrueSD2ArchiveEnv(lookup, sd2ArchiveEnvVersioningVerified) {
		return nil, fmt.Errorf("%s must be explicitly set to true", sd2ArchiveEnvVersioningVerified)
	}
	if !requiredTrueSD2ArchiveEnv(lookup, sd2ArchiveEnvLifecycleVerified) {
		return nil, fmt.Errorf("%s must be explicitly set to true", sd2ArchiveEnvLifecycleVerified)
	}
	retentionText, err := required(sd2ArchiveEnvRetentionDays)
	if err != nil {
		return nil, err
	}
	retentionDays, err := strconv.Atoi(retentionText)
	if err != nil || retentionDays < 1 || retentionDays > 3650 {
		return nil, fmt.Errorf("%s must be between 1 and 3650", sd2ArchiveEnvRetentionDays)
	}

	allowlistText, err := required(sd2ArchiveEnvHostAllowlist)
	if err != nil {
		return nil, err
	}
	resultHosts, err := parseSD2ArchiveHostAllowlist(allowlistText)
	if err != nil {
		return nil, err
	}

	expectedBucketOwner := strings.TrimSpace(optionalSD2ArchiveEnv(lookup, sd2ArchiveEnvExpectedBucketOwner))
	if len(expectedBucketOwner) > 256 || strings.ContainsAny(expectedBucketOwner, "\x00\r\n") {
		return nil, fmt.Errorf("%s is invalid", sd2ArchiveEnvExpectedBucketOwner)
	}

	return &sd2S3ArchiveConfig{
		Endpoint:            endpoint,
		Region:              region,
		Bucket:              bucket,
		AccessKeyID:         accessKeyID,
		SecretAccessKey:     secretAccessKey,
		SessionToken:        sessionToken,
		Prefix:              prefix,
		UsePathStyle:        usePathStyle,
		ExpectedBucketOwner: expectedBucketOwner,
		SSE:                 sse,
		KMSKeyID:            kmsKeyID,
		IdentityHMACKey:     []byte(identityHMACKey),
		TempDir:             tempDir,
		RetentionDays:       retentionDays,
		ResultHosts:         resultHosts,
	}, nil
}

func optionalSD2ArchiveEnv(lookup sd2ArchiveEnvLookup, key string) string {
	value, _ := lookup(key)
	return value
}

func requiredTrueSD2ArchiveEnv(lookup sd2ArchiveEnvLookup, key string) bool {
	value, ok := lookup(key)
	return ok && strings.EqualFold(strings.TrimSpace(value), "true")
}

func isFilesystemRoot(path string) bool {
	volume := filepath.VolumeName(path)
	root := string(filepath.Separator)
	if volume != "" {
		root = volume + string(filepath.Separator)
	}
	return filepath.Clean(path) == filepath.Clean(root)
}

func validSD2ArchiveBucketName(value string) bool {
	if len(value) < 3 || len(value) > 63 || net.ParseIP(value) != nil || !isLowerAlphaNumeric(value[0]) || !isLowerAlphaNumeric(value[len(value)-1]) || strings.Contains(value, "..") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			continue
		}
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
}

func isLowerAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func validSD2ArchivePrefix(value string) bool {
	if len(value) < 1 || len(value) > 128 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, r := range segment {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				continue
			}
			return false
		}
	}
	return true
}

func parseSD2ArchiveHostAllowlist(raw string) (map[string]map[int][]string, error) {
	var document map[string]map[string][]string
	if err := common.Unmarshal([]byte(raw), &document); err != nil {
		return nil, fmt.Errorf("%s must be valid JSON", sd2ArchiveEnvHostAllowlist)
	}
	result := make(map[string]map[int][]string)
	entryCount := 0
	for providerText, channels := range document {
		provider := strings.ToLower(strings.TrimSpace(providerText))
		if !validSD2ArchiveProvider(provider) || len(channels) == 0 {
			return nil, fmt.Errorf("%s contains an invalid provider entry", sd2ArchiveEnvHostAllowlist)
		}
		if _, exists := result[provider]; exists {
			return nil, fmt.Errorf("%s contains a duplicate provider entry", sd2ArchiveEnvHostAllowlist)
		}
		result[provider] = make(map[int][]string)
		for channelText, configuredHosts := range channels {
			channelID, err := strconv.Atoi(channelText)
			if err != nil || channelID <= 0 || channelText != strconv.Itoa(channelID) || len(configuredHosts) == 0 {
				return nil, fmt.Errorf("%s contains an invalid channel entry", sd2ArchiveEnvHostAllowlist)
			}
			if _, exists := result[provider][channelID]; exists {
				return nil, fmt.Errorf("%s contains a duplicate channel entry", sd2ArchiveEnvHostAllowlist)
			}
			seen := make(map[string]struct{})
			hosts := make([]string, 0, len(configuredHosts))
			for _, configuredHost := range configuredHosts {
				host := strings.ToLower(strings.TrimSpace(configuredHost))
				if !validSD2ArchiveHostname(host) {
					return nil, fmt.Errorf("%s contains an invalid result host", sd2ArchiveEnvHostAllowlist)
				}
				if _, exists := seen[host]; exists {
					continue
				}
				seen[host] = struct{}{}
				hosts = append(hosts, host)
			}
			if len(hosts) == 0 {
				return nil, fmt.Errorf("%s contains an empty result host entry", sd2ArchiveEnvHostAllowlist)
			}
			result[provider][channelID] = hosts
			entryCount++
		}
	}
	if entryCount == 0 {
		return nil, fmt.Errorf("%s must contain at least one exact provider/channel entry", sd2ArchiveEnvHostAllowlist)
	}
	return result, nil
}

func validSD2ArchiveProvider(provider string) bool {
	if provider == "" || len(provider) > 64 {
		return false
	}
	for _, r := range provider {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func validSD2ArchiveHostname(host string) bool {
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") || strings.ContainsAny(host, "/\\:@?#%[]") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	parsed, err := url.Parse("https://" + host)
	return err == nil && parsed.Hostname() == host && parsed.Port() == "" && parsed.Path == "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func (c sd2S3ArchiveConfig) resultHostResolver() SD2ResultHostAllowlistResolver {
	resultHosts := c.ResultHosts
	return func(submission *model.TaskSubmission) []string {
		if submission == nil {
			return nil
		}
		provider := strings.ToLower(strings.TrimSpace(submission.Provider))
		channels := resultHosts[provider]
		hosts := channels[submission.ChannelID]
		return append([]string(nil), hosts...)
	}
}

func newSD2S3Client(config sd2S3ArchiveConfig) *s3.Client {
	netDialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           netDialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   20 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	awsConfig := aws.Config{
		Region:                     config.Region,
		Credentials:                aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(config.AccessKeyID, config.SecretAccessKey, config.SessionToken)),
		HTTPClient:                 httpClient,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	return s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(config.Endpoint)
		options.UsePathStyle = config.UsePathStyle
	})
}
