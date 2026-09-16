package panelapi

import (
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

func nowMeta() metav1.Time { return metav1.Now() }
