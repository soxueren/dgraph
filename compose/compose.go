/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"os/user"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v2"

	"github.com/dgraph-io/dgraph/x"
)

type StringMap map[string]string

type Volume struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool `yaml:"read_only"`
}

type Service struct {
	name          string // not exported
	Image         string
	ContainerName string   `yaml:"container_name"`
	WorkingDir    string   `yaml:"working_dir"`
	DependsOn     []string `yaml:"depends_on,omitempty"`
	Labels        StringMap
	Environment   []string
	Ports         []string
	Volumes       []Volume
	TempFS        []string `yaml:",omitempty"`
	User          string   `yaml:",omitempty"`
	Command       string
}

type ComposeConfig struct {
	Version  string
	Services map[string]Service
	Volumes  map[string]StringMap
}

type Options struct {
	NumZeros       int
	NumAlphas      int
	NumGroups      int
	LruSizeMB      int
	EnterpriseMode bool
	AclSecret      string
	DataDir        string
	DataVol        bool
	TempFS         bool
	UserOwnership  bool
	Jaeger         bool
	TestPortRange  bool
	Verbosity      int
	OutFile        string
}

var opts Options

const (
	zeroBasePort  int = 5080
	alphaBasePort int = 7080
)

func name(prefix string, idx int) string {
	return fmt.Sprintf("%s%d", prefix, idx)
}

func toExposedPort(i int) string {
	return fmt.Sprintf("%d:%d", i, i)
}

func initService(basename string, idx, grpcPort int) Service {
	var svc Service

	svc.name = name(basename, idx)
	svc.Image = "dgraph/dgraph:latest"
	svc.ContainerName = svc.name
	svc.WorkingDir = fmt.Sprintf("/data/%s", svc.name)
	if idx > 1 {
		svc.DependsOn = append(svc.DependsOn, name(basename, idx-1))
	}
	svc.Labels = map[string]string{"cluster": "test"}

	svc.Ports = []string{
		toExposedPort(grpcPort),
		toExposedPort(grpcPort + 1000), // http port
	}

	svc.Volumes = append(svc.Volumes, Volume{
		Type:     "bind",
		Source:   "$GOPATH/bin",
		Target:   "/gobin",
		ReadOnly: true,
	})

	switch {
	case opts.DataVol == true:
		svc.Volumes = append(svc.Volumes, Volume{
			Type:     "volume",
			Source:   "data",
			Target:   "/data",
			ReadOnly: false,
		})
	case opts.DataDir != "":
		svc.Volumes = append(svc.Volumes, Volume{
			Type:     "bind",
			Source:   opts.DataDir,
			Target:   "/data",
			ReadOnly: false,
		})
	default:
		// no data volume
	}

	svc.Command = "/gobin/dgraph"
	if opts.UserOwnership {
		user, err := user.Current()
		if err != nil {
			x.CheckfNoTrace(x.Errorf("unable to get current user: %v", err))
		}
		svc.User = fmt.Sprintf("${UID:-%s}", user.Uid)
		svc.WorkingDir = fmt.Sprintf("/working/%s", svc.name)
		svc.Command += fmt.Sprintf(" --cwd=/data/%s", svc.name)
	}
	if opts.Jaeger {
		svc.Command += " --jaeger.collector=http://jaeger:14268"
	}

	return svc
}

func getOffset(idx int) int {
	if idx == 1 {
		return 0
	}
	return idx
}

func getZero(idx int) Service {
	basename := "zero"
	grpcPort := zeroBasePort + getOffset(idx)

	svc := initService(basename, idx, grpcPort)

	if opts.TempFS {
		svc.TempFS = append(svc.TempFS, fmt.Sprintf("/data/%s/zw", svc.name))
	}

	svc.Command += fmt.Sprintf(" zero -o %d --idx=%d", idx-1, idx)
	svc.Command += fmt.Sprintf(" --my=%s:%d", svc.name, grpcPort)
	svc.Command += fmt.Sprintf(" --replicas=%d",
		int(math.Ceil(float64(opts.NumAlphas)/float64(opts.NumGroups))))
	svc.Command += fmt.Sprintf(" --logtostderr -v=%d", opts.Verbosity)
	if idx == 1 {
		svc.Command += fmt.Sprintf(" --bindall")
	} else {
		svc.Command += fmt.Sprintf(" --peer=%s:%d", name(basename, 1), zeroBasePort)
	}

	return svc
}

func getAlpha(idx int) Service {
	baseOffset := 0
	if opts.TestPortRange {
		baseOffset += 100
	}

	basename := "alpha"
	internalPort := alphaBasePort + baseOffset + getOffset(idx)
	grpcPort := internalPort + 1000

	svc := initService(basename, idx, grpcPort)

	if opts.TempFS {
		svc.TempFS = append(svc.TempFS, fmt.Sprintf("/data/%s/w", svc.name))
	}

	svc.Command += fmt.Sprintf(" alpha -o %d", baseOffset+idx-1)
	svc.Command += fmt.Sprintf(" --my=%s:%d", svc.name, internalPort)
	svc.Command += fmt.Sprintf(" --lru_mb=%d", opts.LruSizeMB)
	svc.Command += fmt.Sprintf(" --zero=zero1:%d", zeroBasePort)
	svc.Command += fmt.Sprintf(" --logtostderr -v=%d", opts.Verbosity)
	svc.Command += " --whitelist=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
	if opts.EnterpriseMode {
		svc.Command += " --enterprise_features"
		if opts.AclSecret != "" {
			svc.Command += " --acl_secret_file=/secret/hmac --acl_access_ttl 10s"
			svc.Volumes = append(svc.Volumes, Volume{
				Type:     "bind",
				Source:   opts.AclSecret,
				Target:   "/secret/hmac",
				ReadOnly: true,
			})
		}
	}

	return svc
}

func getJaeger() Service {
	svc := Service{
		Image:         "jaegertracing/all-in-one:latest",
		ContainerName: "jaeger",
		WorkingDir:    "/working/jaeger",
		Ports: []string{
			toExposedPort(16686),
		},
		Environment: []string{"COLLECTOR_ZIPKIN_HTTP_PORT=9411"},
		Command:     "--memory.max-traces=1000000",
	}
	return svc
}

func warning(str string) {
	fmt.Fprintf(os.Stderr, "compose: %v\n", str)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "compose: %v\n", err)
	os.Exit(1)
}

func main() {
	var cmd = &cobra.Command{
		Use:     "compose",
		Short:   "docker-compose config file generator for dgraph",
		Long:    "Dynamically generate a docker-compose.yml file for running a dgraph cluster.",
		Example: "$ compose --num_zeros=3 --num_alphas=3 | docker-compose -f- up",
		Run: func(cmd *cobra.Command, args []string) {
			// dummy to get "Usage:" template in Usage() output.
		},
	}

	cmd.PersistentFlags().IntVarP(&opts.NumZeros, "num_zeros", "z", 3,
		"number of zeros in dgraph cluster")
	cmd.PersistentFlags().IntVarP(&opts.NumAlphas, "num_alphas", "a", 3,
		"number of alphas in dgraph cluster")
	cmd.PersistentFlags().IntVarP(&opts.NumGroups, "num_groups", "g", 1,
		"number of groups in dgraph cluster")
	cmd.PersistentFlags().IntVar(&opts.LruSizeMB, "lru_mb", 1024,
		"approximate size of LRU cache")
	cmd.PersistentFlags().BoolVarP(&opts.DataVol, "data_vol", "o", false,
		"mount a docker volume as /data in containers")
	cmd.PersistentFlags().StringVarP(&opts.DataDir, "data_dir", "d", "",
		"mount a host directory as /data in containers")
	cmd.PersistentFlags().BoolVarP(&opts.EnterpriseMode, "enterprise", "e", false,
		"enable enterprise features in alphas")
	cmd.PersistentFlags().StringVar(&opts.AclSecret, "acl_secret", "",
		"enable ACL feature with specified HMAC secret file")
	cmd.PersistentFlags().BoolVarP(&opts.UserOwnership, "user", "u", false,
		"run as the current user rather than root")
	cmd.PersistentFlags().BoolVar(&opts.TempFS, "tmpfs", false,
		"store w and zw directories on a tmpfs filesystem")
	cmd.PersistentFlags().BoolVarP(&opts.Jaeger, "jaeger", "j", false,
		"include jaeger service")
	cmd.PersistentFlags().BoolVar(&opts.TestPortRange, "test_ports", true,
		"use alpha ports expected by regression tests")
	cmd.PersistentFlags().IntVarP(&opts.Verbosity, "verbosity", "v", 2,
		"glog verbosity level")
	cmd.PersistentFlags().StringVarP(&opts.OutFile, "out", "O", "./docker-compose.yml",
		"name of output file")

	err := cmd.ParseFlags(os.Args)
	if err != nil {
		if err == pflag.ErrHelp {
			cmd.Usage()
			os.Exit(0)
		}
		fatal(err)
	}

	// Do some sanity checks.
	if opts.NumZeros < 1 || opts.NumZeros > 99 {
		fatal(fmt.Errorf("number of zeros must be 1-99"))
	}
	if opts.NumAlphas < 1 || opts.NumAlphas > 99 {
		fatal(fmt.Errorf("number of alphas must be 1-99"))
	}
	if opts.LruSizeMB < 1024 {
		fatal(fmt.Errorf("LRU cache size must be >= 1024 MB"))
	}
	if opts.AclSecret != "" && !opts.EnterpriseMode {
		warning("adding --enterprise because it is required by ACL feature")
		opts.EnterpriseMode = true
	}
	if opts.DataVol && opts.DataDir != "" {
		fatal(fmt.Errorf("only one of --data_vol and --data_dir may be used at a time"))
	}
	if opts.UserOwnership && opts.DataDir == "" {
		fatal(fmt.Errorf("--user option requires --data_dir=<path>"))
	}

	services := make(map[string]Service)

	for i := 1; i <= opts.NumZeros; i++ {
		svc := getZero(i)
		services[svc.name] = svc
	}

	for i := 1; i <= opts.NumAlphas; i++ {
		svc := getAlpha(i)
		services[svc.name] = svc
	}

	cfg := ComposeConfig{
		Version:  "3.5",
		Services: services,
		Volumes:  make(map[string]StringMap),
	}

	if opts.DataVol {
		cfg.Volumes["data"] = StringMap{}
	}

	if opts.Jaeger {
		services["jaeger"] = getJaeger()
	}

	yml, err := yaml.Marshal(cfg)
	x.CheckfNoTrace(err)

	var out io.Writer
	if opts.OutFile == "-" {
		out = os.Stdout
	} else {
		fmt.Fprintf(os.Stderr, "writing file: %s\n", opts.OutFile)
		file, err := os.Create(opts.OutFile)
		if err != nil {
			fatal(fmt.Errorf("unable to open file for writing: %+v", err))
		}
		defer func() { x.Ignore(file.Close()) }()

		buf := bufio.NewWriter(file)
		defer func() { x.Ignore(buf.Flush()) }()

		out = buf
	}

	_, _ = fmt.Fprintf(out, "# Auto-generated with: %v\n#\n", os.Args[:])
	_, _ = fmt.Fprintf(out, "%s", yml)
}