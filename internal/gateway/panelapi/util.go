package panelapi

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nonNegative converts a quantity to uint64, counting a negative one as zero.
func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

func nowMeta() metav1.Time { return metav1.Now() }
