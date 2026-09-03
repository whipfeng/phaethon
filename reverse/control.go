package reverse

import ()

// ControlRequest is a command sent from reverse side to registry side
// over the control connection (carried in FrameData payload as JSON).
type ControlRequest struct {
	Cmd              string `json:"cmd"`            // "register"
	Name             string `json:"name,omitempty"` // reverse-side config name (for UI pairing)
	Seq              int    `json:"seq"`            // stable sequence number for this reverse config
	Proto            string `json:"proto"`          // register protocol (socks5/trojan/h_tunnel)
	PreferredPort    int    `json:"preferred_port"` // 0 = auto-allocate
	ListenerProto    string `json:"listener_proto"` // dynamic listener protocol (socks5/trojan/direct)
	ListenerUser     string `json:"listener_user"`
	ListenerPassword string `json:"listener_password"`
	ListenerSNI      string `json:"listener_sni"`
	DirectDstHost    string `json:"direct_dst_host"` // direct type: target host
	DirectDstPort    int    `json:"direct_dst_port"` // direct type: target port
	RegistryProxy    string `json:"registry_proxy"`  // proxy used to reach the registry
	// ReverseID is a globally unique identifier generated and persisted by the
	// reverse client. The registry uses it to maintain stable port allocations.
	ReverseID string `json:"reverse_id"`
}

// ControlReply is a response from registry side back to reverse side.
type ControlReply struct {
	Status  string `json:"status"`  // "ok" | "error"
	Address string `json:"address"` // allocated unique ID for data BIND
	Port    int    `json:"port"`    // actual listening port (informational)
	Error   string `json:"error"`   // error message when status != "ok"
}

const (
	// BindPortControl is the DST.PORT value used in SOCKS5/Trojan BIND
	// to indicate this connection is a control channel (not data).
	BindPortControl = 1

	// BindPortData is the default DST.PORT value (0) indicating a data connection.
	BindPortData = 0
)
