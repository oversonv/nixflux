package nixflux

import "testing"

func FuzzReadDebArNoPanic(f *testing.F) {
	f.Add([]byte("!<arch>\n"))
	f.Add([]byte("invalid"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = readDebAr(raw)
	})
}

func FuzzParseAPTReleaseNoPanic(f *testing.F) {
	f.Add("Architectures: aarch64\nComponents: main\nSHA256:\n")
	f.Add("garbage")
	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = parseAPTRelease(raw)
	})
}

func FuzzParseAPTPackagesNoPanic(f *testing.F) {
	f.Add([]byte("Package: neofetch\nVersion: 7.1.0-1\nArchitecture: all\nFilename: pool/neofetch.deb\nSize: 1\nSHA256: 0000000000000000000000000000000000000000000000000000000000000000\n"))
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = parseAPT_Packages(raw)
	})
}

func FuzzParseSettingsTOMLNoPanic(f *testing.F) {
	f.Add("channels = []\nenabled = []\n")
	f.Add("[[channels]]\nname = \"termux\"\nurl = \"https://example.invalid/repo\"\n")
	f.Add("enabled = true")
	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = parseSettingsTOML(raw)
	})
}

func FuzzCompareDebianVersionsNoPanic(f *testing.F) {
	f.Add("1.0~rc1-1", "1.0-1")
	f.Add("1:1.0-1", "2.0-1")
	f.Add("", "invalid")
	f.Fuzz(func(t *testing.T, a, b string) {
		_, _ = compareDebianVersions(a, b)
	})
}
