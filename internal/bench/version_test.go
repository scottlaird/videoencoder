package bench

import "testing"

func TestParseEncoderVersion(t *testing.T) {
	tests := []struct {
		name, codec, stdout, stderr, want string
	}{
		{
			name:   "x264 from the bitstream, SIMD from the log",
			codec:  "libx264",
			stdout: "\x00\x05x264 - core 165 r3222 b35605a - H.264/MPEG-4 AVC codec - Copyleft",
			stderr: "[libx264 @ 0x906c58a80] using cpu capabilities: ARMv8 NEON DotProd I8MM\n",
			want:   "x264 core 165 r3222 b35605a (ARMv8 NEON DotProd I8MM)",
		},
		{
			name:  "x265 with AVX-512",
			codec: "libx265",
			stderr: "x265 [info]: HEVC encoder version 4.1+1-1d117be\n" +
				"x265 [info]: using cpu capabilities: MMX2 SSE2Fast LZCNT SSSE3 SSE4.2 AVX FMA3 BMI2 AVX2 AVX512\n",
			want: "x265 4.1+1-1d117be (MMX2 SSE2Fast LZCNT SSSE3 SSE4.2 AVX FMA3 BMI2 AVX2 AVX512)",
		},
		{
			name:   "SVT-AV1",
			codec:  "libsvtav1",
			stderr: "Svt[info]: SVT [version]:\tSVT-AV1 Encoder Lib v4.2.0\nSvt[info]: SVT [build]  :\tApple LLVM\n",
			want:   "SVT-AV1 v4.2.0",
		},
		{
			name:   "libaom",
			codec:  "libaom-av1",
			stderr: "  Stream #0:0 -> #0:0 (wrapped_avframe (native) -> av1 (libaom-av1))\n[libaom-av1 @ 0x72d400a80] 3.15.2\n[libaom-av1 @ 0x72d400a80] Neither bitrate nor constrained quality specified\n",
			want:   "libaom 3.15.2",
		},
		{
			name:   "an encoder that says nothing",
			codec:  "librav1e",
			stderr: "Stream mapping:\n",
			want:   "",
		},
		{
			name:   "a known encoder whose output lacks a version",
			codec:  "libx265",
			stderr: "x265 [info]: using cpu capabilities: NEON\n",
			want:   "",
		},
	}
	for _, tt := range tests {
		if got := parseEncoderVersion(tt.codec, tt.stdout, tt.stderr); got != tt.want {
			t.Errorf("%s: parseEncoderVersion() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCPUInfoModel(t *testing.T) {
	cpuinfo := "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD Ryzen Threadripper PRO 5975WX 32-Cores  \nflags\t\t: fpu vme\n\n" +
		"processor\t: 1\nmodel name\t: AMD Ryzen Threadripper PRO 5975WX 32-Cores\n"
	if got, want := cpuinfoModel(cpuinfo), "AMD Ryzen Threadripper PRO 5975WX 32-Cores"; got != want {
		t.Errorf("cpuinfoModel() = %q, want %q", got, want)
	}
	if got := cpuinfoModel("processor\t: 0\nBogoMIPS\t: 50.00\n"); got != "" {
		t.Errorf("cpuinfoModel(no model name) = %q, want \"\"", got)
	}
}
