package guards

import "github.com/xzs/bridge_qoder/internal/protocol"

// protocolParseDSML defers to the protocol package's DSML parser.
func protocolParseDSML(text string) ([]protocol.ParsedToolCall, string) {
	return protocol.ParseDSMLToolCalls(text)
}
