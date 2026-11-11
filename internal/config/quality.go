package config

// QualityScale is how an ffmpeg encoder's quality setting works: the
// flag that sets it, its useful range and step, a typical value, and
// which way is better.
type QualityScale struct {
	Flag    string
	Min     float64
	Max     float64
	Step    float64
	Default float64
	// HigherIsBetter is true for encoders where a larger number means
	// better quality, such as VideoToolbox's -q:v.
	HigherIsBetter bool
}

var qualityScales = map[string]QualityScale{
	"libx264":    {Flag: "-crf", Min: 10, Max: 40, Step: 0.1, Default: 20},
	"libx265":    {Flag: "-crf", Min: 10, Max: 40, Step: 0.1, Default: 20},
	"libsvtav1":  {Flag: "-crf", Min: 10, Max: 63, Step: 1, Default: 32},
	"libaom-av1": {Flag: "-crf", Min: 10, Max: 63, Step: 1, Default: 32},
	"librav1e":   {Flag: "-qp", Min: 20, Max: 255, Step: 1, Default: 80},
	// NVENC takes -cq:v in quarters: 33.0 to 33.2 all encode as 33.0,
	// 33.3 and 33.4 as 33.25, and so on, the same on every clip (H.264,
	// HEVC and AV1 on an RTX 4090), so finer steps only repeat encodes.
	"h264_nvenc":        {Flag: "-cq:v", Min: 15, Max: 51, Step: 0.25, Default: 28},
	"hevc_nvenc":        {Flag: "-cq:v", Min: 15, Max: 51, Step: 0.25, Default: 28},
	"av1_nvenc":         {Flag: "-cq:v", Min: 15, Max: 51, Step: 0.25, Default: 30},
	"h264_videotoolbox": {Flag: "-q:v", Min: 1, Max: 100, Step: 1, Default: 60, HigherIsBetter: true},
	"hevc_videotoolbox": {Flag: "-q:v", Min: 1, Max: 100, Step: 1, Default: 60, HigherIsBetter: true},
}

// DefaultQualityScale returns the quality scale of an ffmpeg encoder
// such as "libx265", if it's a known one.
func DefaultQualityScale(codec string) (QualityScale, bool) {
	q, ok := qualityScales[codec]
	return q, ok
}
