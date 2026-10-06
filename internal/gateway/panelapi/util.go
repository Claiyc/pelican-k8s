package panelapi

import (
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

// nonNegative converts a quantity to uint64, counting a negative one as zero.
func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

func nowMeta() metav1.Time { return metav1.Now() }
