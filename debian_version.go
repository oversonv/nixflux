package nixflux

import (
	"fmt"
	"strings"
)

type debianVersion struct {
	epoch    string
	upstream string
	revision string
	valid    bool
}

func parseDebianVersion(value string) debianVersion {
	if value == "" || value != strings.TrimSpace(value) {
		return debianVersion{}
	}
	for i := 0; i < len(value); i++ {
		if value[i] > 0x7f || value[i] <= 0x20 {
			return debianVersion{}
		}
	}
	v := debianVersion{epoch: "0", revision: "0"}
	if colon := strings.IndexByte(value, ':'); colon >= 0 {
		if colon == 0 {
			return debianVersion{}
		}
		for _, ch := range value[:colon] {
			if ch < '0' || ch > '9' {
				return debianVersion{}
			}
		}
		v.epoch = value[:colon]
		value = value[colon+1:]
	}
	if dash := strings.LastIndexByte(value, '-'); dash >= 0 {
		v.revision = value[dash+1:]
		value = value[:dash]
		if v.revision == "" {
			return debianVersion{}
		}
		for _, ch := range v.revision {
			if !isDebianRevisionChar(ch) {
				return debianVersion{}
			}
		}
	}
	if value == "" || value[0] < '0' || value[0] > '9' || strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return debianVersion{}
	}
	for _, ch := range value {
		if !isDebianUpstreamChar(ch) {
			return debianVersion{}
		}
	}
	v.upstream = value
	v.valid = true
	return v
}

func isDebianUpstreamChar(ch rune) bool {
	return ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || strings.ContainsRune(".+:~-", ch)
}

func isDebianRevisionChar(ch rune) bool {
	return ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || strings.ContainsRune(".+~", ch)
}

func debianCharOrder(ch byte) int {
	switch {
	case ch == '~':
		return -1
	case ch == 0:
		return 0
	case ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z':
		return int(ch)
	default:
		return int(ch) + 256
	}
}

func debianRevisionCompare(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for (i < len(a) && (a[i] < '0' || a[i] > '9')) || (j < len(b) && (b[j] < '0' || b[j] > '9')) {
			var ac, bc byte
			if i < len(a) && (a[i] < '0' || a[i] > '9') {
				ac = a[i]
			}
			if j < len(b) && (b[j] < '0' || b[j] > '9') {
				bc = b[j]
			}
			ao, bo := debianCharOrder(ac), debianCharOrder(bc)
			if ao < bo {
				return -1
			}
			if ao > bo {
				return 1
			}
			if ac != 0 {
				i++
			}
			if bc != 0 {
				j++
			}
		}
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		startI, startJ := i, j
		for i < len(a) && a[i] >= '0' && a[i] <= '9' {
			i++
		}
		for j < len(b) && b[j] >= '0' && b[j] <= '9' {
			j++
		}
		lenI, lenJ := i-startI, j-startJ
		if lenI < lenJ {
			return -1
		}
		if lenI > lenJ {
			return 1
		}
		if lenI > 0 {
			if c := strings.Compare(a[startI:i], b[startJ:j]); c < 0 {
				return -1
			} else if c > 0 {
				return 1
			}
		}
	}
	return 0
}

func compareDebianVersions(a, b string) (int, error) {
	va, vb := parseDebianVersion(a), parseDebianVersion(b)
	if !va.valid || !vb.valid {
		return 0, fmt.Errorf("invalid Debian version comparison: %q, %q", a, b)
	}
	if c := compareNumericStrings(va.epoch, vb.epoch); c != 0 {
		return c, nil
	}
	if c := debianRevisionCompare(va.upstream, vb.upstream); c != 0 {
		return c, nil
	}
	return debianRevisionCompare(va.revision, vb.revision), nil
}

func compareVersions(a, b string) (int, error) {
	if strings.ContainsAny(a+b, ":~-") {
		if cmp, err := compareDebianVersions(a, b); err == nil {
			return cmp, nil
		}
	}
	if parsePEP440(a).valid && parsePEP440(b).valid {
		return comparePEP440(a, b)
	}
	return compareDebianVersions(a, b)
}

func isValidPackageVersion(value string) bool {
	return parsePEP440(value).valid || parseDebianVersion(value).valid
}

func compareVersionConstraint(candidate, wanted, op string) (bool, error) {
	cmp, err := compareVersions(candidate, wanted)
	if err != nil {
		return false, err
	}
	switch op {
	case "==", "=", "===":
		return cmp == 0, nil
	case "!=":
		return cmp != 0, nil
	case ">=":
		return cmp >= 0, nil
	case "<=":
		return cmp <= 0, nil
	case ">", ">>":
		return cmp > 0, nil
	case "<", "<<":
		return cmp < 0, nil
	default:
		return false, fmt.Errorf("unsupported version operator %q", op)
	}
}
