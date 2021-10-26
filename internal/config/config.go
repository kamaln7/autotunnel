package autotunnel

import (
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	LogLevel        string        `mapstructure:"log_level"`
	InactiveTimeout time.Duration `mapstructure:"inactive_timeout"`
	DialTimeout     time.Duration `mapstructure:"dial_timeout"`
	Tunnels         []Tunnel
	InterfaceName   string `mapstructure:"interface_name"`
}

type Tunnel struct {
	Name        string
	Host        string
	JumpHost    string   `mapstructure:"jump_host"`
	JumpCommand []string `mapstructure:"jump_command"`
	LocalPort   string   `mapstructure:"local_port"`
	RemotePort  string   `mapstructure:"remote_port"`
}

func ReadConfig(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	err := v.ReadInConfig()
	if err != nil {
		return nil, err
	}

	var c Config
	err = v.Unmarshal(&c)
	if err != nil {
		return nil, err
	}

	return &c, nil
}
