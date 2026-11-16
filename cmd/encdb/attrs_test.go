package main

import "testing"

func TestAttributes(t *testing.T) {
	for _, tc := range []struct {
		encoder, flagset, args string
		want                   Attrs
	}{
		{"libx265", "x265-medium-10bit-kf10", "-preset medium -pix_fmt yuv420p10le -g 600",
			Attrs{"x265", true, "medium", "10", "kf10", "default"}},
		{"libx264", "x264-veryslow-kf10", "-preset veryslow",
			Attrs{"x264", true, "veryslow", "8", "kf10", "default"}},
		{"libsvtav1", "svtav1-p4-kf10", "-preset 4",
			Attrs{"SVT-AV1", true, "p4", "as source", "kf10", "default"}},
		{"h264_qsv", "h264-qsv-medium-kf10-bpyr", "-preset medium -b_strategy 1",
			Attrs{"Quick Sync", false, "medium", "8", "kf10", "bpyr"}},
		{"hevc_qsv", "hevc-qsv-slow-10bit-kf10-hwdec", "-vf scale_qsv=format=p010",
			Attrs{"Quick Sync", false, "slow", "10", "kf10", "hwdec"}},
		{"hevc_nvenc", "hevc-nvenc-p7-uhq-nocap-10bit-kf10", "-preset p7 -pix_fmt p010le",
			Attrs{"NVENC", false, "p7", "10", "kf10", "uhq"}},
		{"hevc_nvenc", "hevc-nvenc-p4-10bit-kf10", "-preset p4 -pix_fmt p010le",
			Attrs{"NVENC", false, "p4", "10", "kf10", "capped"}},
		{"hevc_videotoolbox", "hevc-videotoolbox", "",
			Attrs{"VideoToolbox", false, "", "as source", "default", "default"}},
		{"weird_enc", "x-y", "",
			Attrs{"weird_enc", false, "", "as source", "default", "default"}},
	} {
		if got := attributes(tc.encoder, tc.flagset, tc.args); got != tc.want {
			t.Errorf("attributes(%q, %q, %q) = %+v, want %+v", tc.encoder, tc.flagset, tc.args, got, tc.want)
		}
	}
}

func TestDerivedNames(t *testing.T) {
	for _, tc := range []struct {
		sample        string
		height        int64
		clip, res     string
		source, input string
	}{
		{"20180529-band@1080p.mov", 0, "band", "1080p", "20180529-band", "20180529-band@1080p"},
		{"20180529-band.mp4", 2160, "band", "4k", "20180529-band", "20180529-band"},
		{"deception1-1080p-30s.mov", 0, "deception1", "1080p", "deception1-1080p-30s", "deception1-1080p-30s"},
		{"20251206-heron3 test 30s@2160p.mov", 0, "heron3", "4k", "20251206-heron3 test 30s", "20251206-heron3 test 30s@2160p"},
		{"odd.mov", 720, "odd", "", "odd", "odd"},
	} {
		if got := clipOf(tc.sample); got != tc.clip {
			t.Errorf("clipOf(%q) = %q, want %q", tc.sample, got, tc.clip)
		}
		if got := resOf(tc.sample, tc.height); got != tc.res {
			t.Errorf("resOf(%q, %d) = %q, want %q", tc.sample, tc.height, got, tc.res)
		}
		if src, inp := stems(tc.sample); src != tc.source || inp != tc.input {
			t.Errorf("stems(%q) = %q, %q, want %q, %q", tc.sample, src, inp, tc.source, tc.input)
		}
	}
}

func TestCodecOf(t *testing.T) {
	for _, tc := range []struct{ encoder, outCodec, want string }{
		{"libx265", "", "hevc"},
		{"av1_qsv", "", "av1"},
		{"h264_nvenc", "", "h264"},
		{"libx264", "hevc", "hevc"}, // what ffprobe saw wins
		{"mpeg4", "", "mpeg4"},
	} {
		if got := codecOf(tc.encoder, tc.outCodec); got != tc.want {
			t.Errorf("codecOf(%q, %q) = %q, want %q", tc.encoder, tc.outCodec, got, tc.want)
		}
	}
}
