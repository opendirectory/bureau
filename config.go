package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Debug      bool
	Daemon     bool
	Server     string
	Binddn     string
	Password   string
	Base       string
	Update     int    `yaml:"update_interval"`
	Host       bool   `yaml:"host_specific_entries"`
	Restart    bool   `yaml:"restart_service_on_change"`
	Override   string `yaml:"override_hostname"`
	HostDN     string
	Local2LDAP bool             `yaml:"sync_local_changes"`
	Kubernetes KubernetesConfig `yaml:"kubernetes"`
	// Disables LDAP server certificate verification. Only use this for
	// testing or servers with self-signed certificates: it allows
	// man-in-the-middle attacks that expose the bind password.
	TLSSkipVerify bool `yaml:"tls_skip_verify"`
}

type KubernetesConfig struct {
	Enabled bool `yaml:"enabled"`
	// Path to a kubeconfig file. Empty = use the pod's service account
	// credentials (in-cluster config).
	Kubeconfig string `yaml:"kubeconfig"`
	// Empty = the pod's namespace (or the kubeconfig context's namespace)
	Namespace string `yaml:"namespace"`
	Prefix    string `yaml:"configmap_prefix"` // empty = "bureau"
}

// configLocations returns the places bureau.yaml is looked for, in order of
// precedence.
func configLocations() []string {
	locations := []string{"bureau.yaml"}

	if exe, err := os.Executable(); err == nil {
		locations = append(locations, filepath.Join(filepath.Dir(exe), "bureau.yaml"))
	}

	if home, err := os.UserHomeDir(); err == nil {
		locations = append(locations,
			filepath.Join(home, ".bureau.yaml"),
			filepath.Join(home, ".config", "bureau", "bureau.yaml"),
		)
	}

	if runtime.GOOS == "windows" {
		// %APPDATA%\bureau\bureau.yaml
		if dir, err := os.UserConfigDir(); err == nil {
			locations = append(locations, filepath.Join(dir, "bureau", "bureau.yaml"))
		}
		// %ProgramData%\bureau\bureau.yaml
		if dir := os.Getenv("ProgramData"); dir != "" {
			locations = append(locations, filepath.Join(dir, "bureau", "bureau.yaml"))
		}
	} else {
		locations = append(locations, "/etc/bureau/bureau.yaml")
	}

	return locations
}

func ConfigInit() (Config, error) {

	var conf string
	var data []byte
	var err error

	if configPath != "" {
		// explicit -config: never fall back to other locations
		conf = configPath
	} else {
		for _, path := range configLocations() {
			if _, err = os.Stat(path); err == nil {
				conf = path
				break
			}
		}
	}
	if conf != "" {
		data, err = os.ReadFile(conf)
		if err != nil {
			return C, fmt.Errorf("reading %s: %w", conf, err)
		}

		if err = yaml.Unmarshal(data, &C); err != nil {
			return C, fmt.Errorf("parsing %s: %w", conf, err)
		}
		Logger(nil, "Using config file "+conf, "DEBUG")
	} else {
		C = Config{
			Debug:    true,
			Daemon:   true,
			Server:   "ldap://opendirectory.net",
			Binddn:   "cn=example,ou=admins,dc=opendirectory,dc=net",
			Password: "Passw0rd",
			Base:     "ou=config,dc=example,dc=opendirectory,dc=net",
			Update:   600,
			Host:     true,
			Restart:  true,
			Override: "bureau",
		}
	}

	// an unset/invalid interval would make daemon mode hammer the LDAP server
	if C.Update <= 0 {
		C.Update = 600
	}

	if C.TLSSkipVerify {
		Logger(fmt.Errorf("tls_skip_verify is enabled"), "LDAP server certificate will NOT be verified", "WARN")
	}

	if C.Debug {
		for i := 0; i <= 9; i++ {
			key := reflect.Indirect(reflect.ValueOf(C)).Type().Field(i).Name
			value := reflect.ValueOf(C)

			if key == "Password" && C.Password != "" {
				Logger(nil, fmt.Sprintf("Loading configuration %v: %s\n", key, "***HIDDEN PASSWORD***"), "DEBUG")
			} else {
				Logger(nil, fmt.Sprintf("Loading configuration %v: %v\n", key, value.FieldByName(key)), "DEBUG")
			}
		}
	}

	// get hostname and set search base
	host, err := os.Hostname()
	if err != nil {
		return C, fmt.Errorf("getting hostname: %w", err)
	}
	Logger(nil, "Getting hostname: "+host, "DEBUG")

	if C.Host {
		if C.Override != "" {
			C.HostDN = "cn=" + C.Override + "," + C.Base
		} else {
			C.HostDN = "cn=" + host + "," + C.Base
		}
	} else {
		C.HostDN = C.Base
	}
	Logger(nil, "Looking for files in: "+C.HostDN, "DEBUG")

	return C, nil
}
