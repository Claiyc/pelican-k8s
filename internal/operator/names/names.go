// Package names centralises the names of objects owned by a GameServer.
package names

import "fmt"

// Prefix is prepended to the server UUID for every owned object.
const Prefix = "gs-"

// ForUUID returns the GameServer name for a Panel server UUID.
func ForUUID(uuid string) string { return Prefix + uuid }

// UUIDFromName extracts the server UUID from a GameServer name.
func UUIDFromName(name string) (string, bool) {
	if len(name) > len(Prefix) && name[:len(Prefix)] == Prefix {
		return name[len(Prefix):], true
	}
	return "", false
}

// PVC is the server volume claim.
func PVC(uuid string) string { return Prefix + uuid }

// EnvSecret holds the egg variables.
func EnvSecret(uuid string) string { return Prefix + uuid + "-env" }

// AgentSecret holds the agent's Wings token.
func AgentSecret(uuid string) string { return Prefix + uuid + "-agent" }

// StatefulSet is the game pod's controller.
func StatefulSet(uuid string) string { return Prefix + uuid }

// Pod is the single pod of the StatefulSet.
func Pod(uuid string) string { return Prefix + uuid + "-0" }

// ExposureService exposes the game ports.
func ExposureService(uuid string) string { return Prefix + uuid }

// AgentService is the headless service for the agent.
func AgentService(uuid string) string { return Prefix + uuid + "-agent" }

// NetworkPolicy is the per-server policy.
func NetworkPolicy(uuid string) string { return Prefix + uuid }

// InstallConfigMap holds the install script of a generation.
func InstallConfigMap(uuid string, gen int64) string {
	return fmt.Sprintf("%s%s-install-%d", Prefix, uuid, gen)
}

// InstallJob runs the install script of a generation.
func InstallJob(uuid string, gen int64) string { return InstallConfigMap(uuid, gen) }

// Short returns the first eight characters of a UUID.
func Short(uuid string) string {
	if len(uuid) < 8 {
		return uuid
	}
	return uuid[:8]
}
