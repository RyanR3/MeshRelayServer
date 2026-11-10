package main

import (
	"encoding/json"
	"os"
)

type Config struct {
	PTTPort               int    `json:"ptt_port"`
	WebPort               int    `json:"web_port"`
	MaxClients            int    `json:"max_clients"`
	MaxChannels           int    `json:"max_channels"`
	LogLevel              string `json:"log_level"`
	AuthEnabled           bool   `json:"auth_enabled"`
	UsersFile             string `json:"users_file"`
	WebAdminsFile         string `json:"web_admins_file"`
	AgenciesFile          string `json:"agencies_file"`
	MutualAidFile         string `json:"mutual_aid_file"`
	LMRBridgeFile         string `json:"lmr_bridge_file"`
	MutualAidRequestsFile string `json:"mutual_aid_requests_file"`
	ZelloBridgesFile      string `json:"zello_bridges_file"`
	GitHubToken           string `json:"github_token,omitempty"`
	// Radio traffic recording (recorder.go). Days to keep; 0 = default 30.
	RecordingDir           string `json:"recording_dir,omitempty"`
	RecordingRetentionDays int    `json:"recording_retention_days,omitempty"`
	DebugPprof             bool   `json:"debug_pprof,omitempty"`
	TLSCertFile            string `json:"tls_cert_file,omitempty"`
	TLSKeyFile             string `json:"tls_key_file,omitempty"`
	// MCX Step 2: the MCPTT domain for this deployment. Per-agency MCX identities
	// are <callsign>@<agencyID>.<mcptt_domain> (e.g. raptor7@sullivan.meshrelayptt.com).
	// Empty disables MCX id derivation (ids fall back to the plain domain).
	McpttDomain string `json:"mcptt_domain,omitempty"`
	// MCX Step 3: agencies with server-enforced floor control (priority preemption,
	// queue, dead-man timer). Channels of agencies NOT listed here stay open PTT.
	// Off by default so existing operations are unaffected until explicitly enabled.
	FloorControlAgencies []string `json:"floor_control_agencies,omitempty"`
	// MCX Step 4: per-channel MCX group type. Keyed by "agencyID:channel". Absent
	// keys default to "preconfigured" (standard floor-controlled channel).
	//   preconfigured — standard (default)
	//   broadcast     — only BroadcastUsers[key] may hold the floor
	//   emergency     — all floor requests get Priority 1
	ChannelGroupTypes map[string]string `json:"channel_group_types,omitempty"`
	// BroadcastUsers maps "agencyID:channel" -> the callsign allowed to transmit on
	// a broadcast-type channel.
	BroadcastUsers map[string]string `json:"broadcast_users,omitempty"`
	// MCX Step 5: SIP gateway to an external MCX platform (FirstNet PTT, carrier
	// MCPTT). Disabled by default — zero impact until an interop requirement exists.
	SipGateway *SipGatewayConfig `json:"sip_gateway,omitempty"`
}

// SipGatewayConfig configures the MCX SIP gateway (Step 5 stub).
type SipGatewayConfig struct {
	Enabled         bool   `json:"enabled"`
	SipPort         int    `json:"sip_port"`          // default 5060
	McxPlatformHost string `json:"mcx_platform_host"` // external MCX SIP host:port
	McxDomain       string `json:"mcx_domain"`
	GatewayMcxID    string `json:"gateway_mcx_id"` // SIP URI for this gateway
	RtpPortRange    string `json:"rtp_port_range"` // e.g. "20000-20100"
	TlsEnabled      bool   `json:"tls_enabled"`
	// BridgeAgency/BridgeChannel: which MeshRelay agency:channel the gateway bridges
	// to the external MCX group. Audio/floor cross this channel.
	BridgeAgency  string `json:"bridge_agency"`
	BridgeChannel int    `json:"bridge_channel"`
}

func DefaultConfig() *Config {
	return &Config{
		PTTPort:               5000,
		WebPort:               8080,
		MaxClients:            1000,
		MaxChannels:           100,
		LogLevel:              "INFO",
		AuthEnabled:           true,
		UsersFile:             "users.json",
		WebAdminsFile:         "web_admins.json",
		AgenciesFile:          "agencies.json",
		MutualAidFile:         "mutual_aid.json",
		LMRBridgeFile:         "lmr_bridges.json",
		MutualAidRequestsFile: "mutual_aid_requests.json",
		ZelloBridgesFile:      "zello_bridges.json",
		McpttDomain:           "meshrelayptt.com",
	}
}

func LoadConfig(filename string) (*Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	config := DefaultConfig()
	decoder := json.NewDecoder(file)
	err = decoder.Decode(config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func (c *Config) Save(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(c)
}
