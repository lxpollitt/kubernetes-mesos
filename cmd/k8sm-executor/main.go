package main

import (
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/version/verflag"
	"github.com/mesosphere/kubernetes-mesos/pkg/executor/service"
	"github.com/spf13/pflag"
)

func main() {
	s := service.NewKubeletExecutorServer()
	s.AddFlags(pflag.CommandLine)

	util.InitFlags()
	util.InitLogs()
	defer util.FlushLogs()

	verflag.PrintAndExitIfRequested()

	s.Run(pflag.CommandLine.Args())
}
