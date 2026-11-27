package nixnet

import (
	"fmt"
	"regexp"
	"strings"
)

var pep440Pattern = regexp.MustCompile(`(?i)^v?(?:(\d+)!)?(\d+(?:\.\d+)*)(?:(?:[-_.]?)(a|b|c|rc|alpha|beta|pre|preview)(?:[-_.]?(\d+))?)?(?:(?:-(\d+))|(?:[-_.]?(post|rev|r)(?:[-_.]?(\d+))?))?(?:[-_.]?(dev)(?:[-_.]?(\d+))?)?(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?$`)

type pepVersion struct {
	epoch   string
	release []string
	preRank int
	preNum  string
	post    *string
	dev     *string
	local   []string
	valid   bool
}

func parsePEP440(value string) pepVersion {
	if value != strings.TrimSpace(value) {
		return pepVersion{}
	}
	m := pep440Pattern.FindStringSubmatch(value)
	if m == nil {
		return pepVersion{}
	}
	v := pepVersion{epoch: m[1], release: strings.Split(m[2], "."), preRank: 3, preNum: "0", valid: true}
	if v.epoch == "" {
		v.epoch = "0"
	}
	if m[3] != "" {
		switch strings.ToLower(m[3]) {
		case "a", "alpha":
			v.preRank = 0
		case "b", "beta":
			v.preRank = 1
		default:
			v.preRank = 2
		}
		if m[4] != "" {
			v.preNum = m[4]
		}
	} else if m[8] != "" {
		v.preRank = -1
	}
	post := m[5]
	if post == "" && m[6] != "" {
		post = m[7]
		if post == "" {
			post = "0"
		}
	}
	if post != "" {
		v.post = &post
	}
	if m[8] != "" {
		dev := m[9]
		if dev == "" {
			dev = "0"
		}
		v.dev = &dev
	}
	if m[10] != "" {
		for _, part := range regexp.MustCompile(`[-_.]`).Split(strings.ToLower(m[10]), -1) {
			v.local = append(v.local, part)
		}
	}
	return v
}

func isValidVersion(version string) bool {
	for i := 0; i < len(version); i++ {
		if version[i] > 0x7f {
			return false
		}
	}
	return parsePEP440(version).valid || parseDebianVersion(version).valid
}

func compareNumericStrings(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

func compareOptionalNumber(a, b *string, absentLow bool) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		if absentLow {
			return -1
		}
		return 1
	}
	if b == nil {
		if absentLow {
			return 1
		}
		return -1
	}
	return compareNumericStrings(*a, *b)
}

func compareParsedPEP440(va, vb pepVersion) int {
	if c := compareNumericStrings(va.epoch, vb.epoch); c != 0 {
		return c
	}
	max := len(va.release)
	if len(vb.release) > max {
		max = len(vb.release)
	}
	for i := 0; i < max; i++ {
		x, y := "0", "0"
		if i < len(va.release) {
			x = va.release[i]
		}
		if i < len(vb.release) {
			y = vb.release[i]
		}
		if c := compareNumericStrings(x, y); c != 0 {
			return c
		}
	}
	if va.preRank < vb.preRank {
		return -1
	}
	if va.preRank > vb.preRank {
		return 1
	}
	if c := compareNumericStrings(va.preNum, vb.preNum); c != 0 {
		return c
	}
	if c := compareOptionalNumber(va.post, vb.post, true); c != 0 {
		return c
	}
	if c := compareOptionalNumber(va.dev, vb.dev, false); c != 0 {
		return c
	}
	if len(va.local) == 0 && len(vb.local) != 0 {
		return -1
	}
	if len(va.local) != 0 && len(vb.local) == 0 {
		return 1
	}
	for i := 0; i < len(va.local) && i < len(vb.local); i++ {
		x, y := va.local[i], vb.local[i]
		xNumeric := regexp.MustCompile(`^[0-9]+$`).MatchString(x)
		yNumeric := regexp.MustCompile(`^[0-9]+$`).MatchString(y)
		var c int
		if xNumeric && yNumeric {
			c = compareNumericStrings(x, y)
		} else if xNumeric {
			c = 1
		} else if yNumeric {
			c = -1
		} else {
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	if len(va.local) < len(vb.local) {
		return -1
	}
	if len(va.local) > len(vb.local) {
		return 1
	}
	return 0
}

func comparePEP440(a, b string) (int, error) {
	va, vb := parsePEP440(a), parsePEP440(b)
	if !va.valid || !vb.valid {
		return 0, fmt.Errorf("invalid PEP 440 version comparison: %q, %q", a, b)
	}
	return compareParsedPEP440(va, vb), nil
}

func versionsEqual(a, b string) bool {
	cmp, err := compareVersions(a, b)
	return err == nil && cmp == 0
}

func isPEP440Prerelease(value string) bool {
	v := parsePEP440(value)
	return v.valid && (v.preRank < 3 || v.dev != nil)
}

func sameRelease(a, b pepVersion) bool {
	if compareNumericStrings(a.epoch, b.epoch) != 0 {
		return false
	}
	max := len(a.release)
	if len(b.release) > max {
		max = len(b.release)
	}
	for i := 0; i < max; i++ {
		x, y := "0", "0"
		if i < len(a.release) {
			x = a.release[i]
		}
		if i < len(b.release) {
			y = b.release[i]
		}
		if compareNumericStrings(x, y) != 0 {
			return false
		}
	}
	return true
}

func versionMatchesReleasePrefix(version string, prefix pepVersion, count int) (bool, error) {
	candidate := parsePEP440(version)
	if !candidate.valid || !prefix.valid || count < 1 || len(candidate.release) < count || len(prefix.release) < count {
		return false, fmt.Errorf("invalid PEP 440 release prefix")
	}
	if compareNumericStrings(candidate.epoch, prefix.epoch) != 0 {
		return false, nil
	}
	for i := 0; i < count; i++ {
		if compareNumericStrings(candidate.release[i], prefix.release[i]) != 0 {
			return false, nil
		}
	}
	return true, nil
}
