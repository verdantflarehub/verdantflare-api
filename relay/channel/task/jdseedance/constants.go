package jdseedance

const (
	ChannelName       = "jd-seedance"
	ModelJDSeedanceSD = "jd-seedance-sd2"

	defaultBaseURL = "https://agentrs.jd.com"
	createPath     = "/api/saas/plugin-u/v1/exec/dance-create"
	queryPath      = "/api/saas/plugin-u/v1/exec/dance-query"

	contentTypeText     = "text"
	contentTypeImageURL = "image_url"
	contentTypeVideoURL = "video_url"
	contentTypeAudioURL = "audio_url"
	defaultImageRole    = "reference_image"
	maxVideoReferences  = 3
	defaultRatio        = "16:9"
)

var ModelList = []string{
	ModelJDSeedanceSD,
}

var allowedRatios = map[string]struct{}{
	"16:9": {},
	"9:16": {},
	"1:1":  {},
	"4:3":  {},
	"3:4":  {},
}
