package main

import (
	"encoding/json"
	"os"
)

type Config struct {
	PTTPort     int    `json:"ptt_port"`
	WebPort     int    `json:"web_port"`
	MaxClients  int    `json:"max_clients"`
	MaxChannels int    `json:"max_channels"`
	LogLevel    string `json:"log_level"`
	AuthEnabled   bool   `json:"auth_enabled"`
	UsersFile     string `json:"users_file"`
	WebAdminsFile string `json:"web_admins_file"`
	AgenciesFile   string `json:"agencies_file"`
	MutualAidFile  string `json:"mutual_aid_file"`
	GitHubToken    string `json:"github_token,omitempty"`
	DebugPprof     bool   `json:"debug_pprof,omitempty"`
	TLSCertFile    string `json:"tls_cert_file,omitempty"`
	TLSKeyFile     string `json:"tls_key_file,omitempty"`
}

func DefaultConfig() *Config {
	return &Config{
		PTTPort:     5000,
		WebPort:     8080,
		MaxClients:  1000,
		MaxChannels: 100,
		LogLevel:    "INFO",
		AuthEnabled:   true,
		UsersFile:     "users.json",
		WebAdminsFile: "web_admins.json",
		AgenciesFile:   "agencies.json",
		MutualAidFile:  "mutual_aid.json",
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
