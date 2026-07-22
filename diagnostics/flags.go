package diagnostics

import (
	"os"
	"strings"
)

// Diagnostic flags are read once at startup.
// Raw chunk logs require a separate opt-in.
var (
	masterEnabled = envBool("KIRO_DIAGNOSTICS")

	streamEnabled = masterEnabled ||
		envBool("KIRO_DIAG_STREAM")

	payloadEnabled = masterEnabled ||
		envBool("KIRO_DIAG_PAYLOAD")

	reasoningEnabled = masterEnabled ||
		envBool("KIRO_DIAG_REASONING")

	chunksEnabled = envBool("KIRO_DIAG_CHUNKS")
)

func envBool(name string) bool {
	value, exists := os.LookupEnv(name)
	if !exists {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

func Stream() bool {
	return streamEnabled
}

func Payload() bool {
	return payloadEnabled
}

func Reasoning() bool {
	return reasoningEnabled
}

func Chunks() bool {
	return chunksEnabled
}

func Any() bool {
	return streamEnabled ||
		payloadEnabled ||
		reasoningEnabled ||
		chunksEnabled
}
