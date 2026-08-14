package domain

import "strings"

// ManagedIdentity derives the immutable Docker Compose and directory identities
// owned by one Fleet instance. Callers must validate instanceID and name before
// using the returned values as resource names.
func ManagedIdentity(instanceID, name string) (projectName, dataVolume, directoryName string) {
	shortID := strings.ReplaceAll(instanceID, "-", "")
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	directoryName = name + "-" + shortID
	projectName = "hermes-fleet-" + directoryName
	dataVolume = projectName + "-data"
	return projectName, dataVolume, directoryName
}
