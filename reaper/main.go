package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	joonix "github.com/joonix/log"
	"github.com/sirupsen/logrus"
)

const envLogLevel = "LOG_LEVEL"
const envLogFormat = "LOG_FORMAT"
const fluentdFormat = "Fluentd"
const logrusFormat = "Logrus"
const defaultLogLevel = logrus.InfoLevel

func main() {
	logLevel := getLogLevel()
	logrus.SetLevel(logLevel)
	logFormat := getLogFormat()
	logrus.SetFormatter(logFormat)

	// Create cancellable context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigChan
		logrus.WithField("signal", sig.String()).Info("received shutdown signal")
		cancel()
	}()

	reaper := newReaper()
	reaper.harvest(ctx)
	logrus.Info("pod reaper is exiting")
}

func getLogLevel() logrus.Level {
	levelString, exists := os.LookupEnv(envLogLevel)
	if !exists {
		return defaultLogLevel
	}

	level, err := logrus.ParseLevel(levelString)
	if err != nil {
		logrus.WithError(err).WithField("env_var", envLogLevel).Error("error parsing log level")
		return defaultLogLevel
	}

	return level
}

func getLogFormat() logrus.Formatter {
	formatString, exists := os.LookupEnv(envLogFormat)
	if !exists || formatString == logrusFormat {
		return &logrus.JSONFormatter{}
	} else if formatString == fluentdFormat {
		return joonix.NewFormatter()
	} else {
		logrus.WithFields(logrus.Fields{
			"env_var": envLogFormat,
			"value":   formatString,
		}).Error("unknown log format")
		return &logrus.JSONFormatter{}
	}
}
