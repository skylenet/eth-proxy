package main

import (
	"os"
	"os/signal"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/skylenet/eth-proxy/pkg/proxy"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "eth-proxy",
	Short: "Reverse proxy for ethereum nodes",
	Run: func(cmd *cobra.Command, args []string) {
		cfg := initCommon()
		p := proxy.NewProxy(cfg)
		if err := p.Serve(); err != nil {
			log.WithError(err).Fatal("failed to serve proxy server")
		}
	},
}

var (
	cfgFile string
)

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "config.yaml", "config file (default is config.yaml)")
}

func loadConfigFromFile(file string) (*proxy.Config, error) {
	if file == "" {
		file = "config.yaml"
	}

	config := &proxy.Config{}

	yamlFile, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	if err := yaml.Unmarshal(yamlFile, &config); err != nil {
		return nil, err
	}

	return config, nil
}

func initCommon() *proxy.Config {
	log.SetFormatter(&log.TextFormatter{})

	log.WithField("cfgFile", cfgFile).Info("loading config")

	config, err := loadConfigFromFile(cfgFile)
	if err != nil {
		log.Fatal(err)
	}

	logLevel, err := log.ParseLevel(config.GlobalConfig.LoggingLevel)
	if err != nil {
		log.WithField("logLevel", config.GlobalConfig.LoggingLevel).Fatal("invalid logging level")
	}

	log.SetLevel(logLevel)

	return config
}

func main() {
	cancel := make(chan os.Signal, 1)
	signal.Notify(cancel, syscall.SIGTERM, syscall.SIGINT)

	go Execute()

	sig := <-cancel
	log.Printf("Caught signal: %v", sig)

	os.Exit(0)
}
