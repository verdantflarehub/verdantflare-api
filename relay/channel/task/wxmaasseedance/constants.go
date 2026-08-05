package wxmaasseedance

import "time"

const (
	ChannelName = "wxmaas-seedance"

	PublicModel   = "verdantflare-sd2"
	UpstreamModel = "doubao-seedance-2.0"

	DefaultBaseURL      = "https://wxmaas.clarmic.com"
	DefaultModelMapping = `{"verdantflare-sd2":"doubao-seedance-2.0"}`

	createPath      = "/v1/video/generations"
	queryPathPrefix = "/v1/video/tasks/"

	contentTypeText     = "text"
	contentTypeImageURL = "image_url"
	contentTypeVideoURL = "video_url"
	contentTypeAudioURL = "audio_url"

	defaultRatio      = "16:9"
	defaultResolution = "720p"
	defaultDuration   = 10
	minDuration       = 4
	maxDuration       = 15
	maxImageCount     = 9
	maxVideoCount     = 3
	maxAudioCount     = 1

	maxProviderResponseBytes = 1 << 20
	providerQueryTimeout     = 30 * time.Second
)

var ModelList = []string{PublicModel}

var allowedRatios = map[string]struct{}{
	"16:9": {},
	"9:16": {},
	"1:1":  {},
	"4:3":  {},
	"3:4":  {},
}
