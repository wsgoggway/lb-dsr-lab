package health

import "fmt"

// Combine folds the local and peer health views into the effective member
// state according to mode:
//
//	"local" — the peer view is ignored, local wins;
//	"and"   — UP iff both local and peer are UP, otherwise DOWN;
//	"or"    — UP iff local or peer is UP, otherwise DOWN.
//
// Any state other than StateUP (i.e. DOWN or INACTIVE) counts as "not up"
// for and/or. Unknown modes yield an error.
func Combine(local, peer string, mode string) (string, error) {
	switch mode {
	case "local":
		return local, nil
	case "and":
		if local == StateUP && peer == StateUP {
			return StateUP, nil
		}
		return StateDOWN, nil
	case "or":
		if local == StateUP || peer == StateUP {
			return StateUP, nil
		}
		return StateDOWN, nil
	default:
		return "", fmt.Errorf("unknown health consensus mode %q (want local|and|or)", mode)
	}
}
