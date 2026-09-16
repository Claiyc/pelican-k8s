package app

import (
	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/rest"
)

type sshSigner = ssh.Signer

var restConfig *rest.Config

// SetRestConfig records the rest config for direct (uncached) clients.
func SetRestConfig(rc *rest.Config) { restConfig = rc }

func clientRestConfig(_ *Gateway) *rest.Config { return restConfig }
