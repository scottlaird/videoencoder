package bench

import "github.com/scottlaird/videoencoder/internal/ffmpeg"

// Progress is how far a running encode or scoring has got, measured
// against the sample's duration.
type Progress = ffmpeg.Progress
