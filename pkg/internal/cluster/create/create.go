/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package create

import (
	"fmt"
	"math/rand"
	"regexp"
	"time"

	"github.com/alessio/shellescape"

	"sigs.k8s.io/kind/pkg/errors"
	"sigs.k8s.io/kind/pkg/internal/apis/config"
	"sigs.k8s.io/kind/pkg/internal/apis/config/encoding"
	"sigs.k8s.io/kind/pkg/internal/cluster/context"
	"sigs.k8s.io/kind/pkg/internal/cluster/delete"
	"sigs.k8s.io/kind/pkg/internal/util/cli"
	"sigs.k8s.io/kind/pkg/log"

	"sigs.k8s.io/kind/pkg/internal/cluster/create/actions"
	configaction "sigs.k8s.io/kind/pkg/internal/cluster/create/actions/config"
	"sigs.k8s.io/kind/pkg/internal/cluster/create/actions/installcni"
	"sigs.k8s.io/kind/pkg/internal/cluster/create/actions/installstorage"
	"sigs.k8s.io/kind/pkg/internal/cluster/create/actions/kubeadminit"
	"sigs.k8s.io/kind/pkg/internal/cluster/create/actions/kubeadmjoin"
	"sigs.k8s.io/kind/pkg/internal/cluster/create/actions/loadbalancer"
	"sigs.k8s.io/kind/pkg/internal/cluster/create/actions/waitforready"
	"sigs.k8s.io/kind/pkg/internal/cluster/kubeconfig"
)

const (
	// Typical host name max limit is 64 characters (https://linux.die.net/man/2/sethostname)
	// We append -control-plane (14 characters) to the cluster name on the control plane container
	clusterNameMax = 50
)

// similar to valid docker container names, but since we will prefix
// and suffix this name, we can relax it a little
// see NewContext() for usage
// https://godoc.org/github.com/docker/docker/daemon/names#pkg-constants
var validNameRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// ClusterOptions holds cluster creation options
type ClusterOptions struct {
	Config *config.Cluster
	// NodeImage overrides the nodes' images in Config if non-zero
	NodeImage      string
	Retain         bool
	WaitForReady   time.Duration
	KubeconfigPath string
	// see https://github.com/kubernetes-sigs/kind/issues/324
	StopBeforeSettingUpKubernetes bool // if false kind should setup kubernetes after creating nodes
	// Options to control output
	DisplayUsage      bool
	DisplaySalutation bool
}

// Cluster creates a cluster
func Cluster(logger log.Logger, ctx *context.Context, opts *ClusterOptions) error {
	// default / process options (namely config)
	if err := fixupOptions(opts); err != nil {
		return err
	}

	// validate the name
	if !validNameRE.MatchString(ctx.Name()) {
		return errors.Errorf(
			"'%s' is not a valid cluster name, cluster names must match `%s`",
			ctx.Name(), validNameRE.String(),
		)
	}
	// warn if cluster name might typically be too long
	if len(ctx.Name()) > clusterNameMax {
		logger.Warnf("cluster name %q is probably too long, this might not work properly on some systems", ctx.Name())
	}

	// then validate
	if err := opts.Config.Validate(); err != nil {
		return err
	}

	// setup a status object to show progress to the user
	status := cli.StatusForLogger(logger)

	// Create node containers implementing defined config Nodes
	if err := ctx.Provider().Provision(status, ctx.Name(), opts.Config); err != nil {
		// In case of errors nodes are deleted (except if retain is explicitly set)
		logger.Errorf("%v", err)
		if !opts.Retain {
			_ = delete.Cluster(logger, ctx, opts.KubeconfigPath)
		}
		return err
	}

	// TODO(bentheelder): make this controllable from the command line?
	actionsToRun := []actions.Action{
		loadbalancer.NewAction(), // setup external loadbalancer
		configaction.NewAction(), // setup kubeadm config
	}
	if !opts.StopBeforeSettingUpKubernetes {
		actionsToRun = append(actionsToRun,
			kubeadminit.NewAction(), // run kubeadm init
		)
		// this step might be skipped, but is next after init
		if !opts.Config.Networking.DisableDefaultCNI {
			actionsToRun = append(actionsToRun,
				installcni.NewAction(), // install CNI
			)
		}
		// add remaining steps
		actionsToRun = append(actionsToRun,
			installstorage.NewAction(),                // install StorageClass
			kubeadmjoin.NewAction(),                   // run kubeadm join
			waitforready.NewAction(opts.WaitForReady), // wait for cluster readiness
		)
	}

	// run all actions
	actionsContext := actions.NewActionContext(logger, opts.Config, ctx, status)
	for _, action := range actionsToRun {
		if err := action.Execute(actionsContext); err != nil {
			if !opts.Retain {
				_ = delete.Cluster(logger, ctx, opts.KubeconfigPath)
			}
			return err
		}
	}

	// skip the rest if we're not setting up kubernetes
	if opts.StopBeforeSettingUpKubernetes {
		return nil
	}

	if err := kubeconfig.Export(ctx, opts.KubeconfigPath); err != nil {
		return err
	}

	// optionally display usage
	if opts.DisplayUsage {
		logUsage(logger, ctx, opts.KubeconfigPath)
	}
	// optionally give the user a friendly salutation
	if opts.DisplaySalutation {
		logger.V(0).Info("")
		logSalutation(logger)
	}
	return nil
}

func logUsage(logger log.Logger, ctx *context.Context, explicitKubeconfigPath string) {
	// construct a sample command for interacting with the cluster
	kctx := kubeconfig.ContextForCluster(ctx.Name())
	sampleCommand := fmt.Sprintf("kubectl cluster-info --context %s", kctx)
	if explicitKubeconfigPath != "" {
		// explicit path, include this
		sampleCommand += " --kubeconfig " + shellescape.Quote(explicitKubeconfigPath)
	}
	logger.V(0).Infof(`Set kubectl context to "%s"`, kctx)
	logger.V(0).Infof("You can now use your cluster with:\n\n" + sampleCommand)
}

func logSalutation(logger log.Logger) {
	salutations := []string{
		"Have a nice day! 👋",
		"Thanks for using kind! 😊",
		"Not sure what to do next? 😅 Check out https://kind.sigs.k8s.io/docs/user/quick-start/",
		"Have a question, bug, or feature request? Let us know! https://kind.sigs.k8s.io/#community 🙂",
	}
	r := rand.New(rand.NewSource(time.Now().UTC().UnixNano()))
	s := salutations[r.Intn(len(salutations))]
	logger.V(0).Info(s)
}

func fixupOptions(opts *ClusterOptions) error {
	// do post processing for options
	// first ensure we at least have a default cluster config
	if opts.Config == nil {
		cfg, err := encoding.Load("")
		if err != nil {
			return err
		}
		opts.Config = cfg
	}

	// if NodeImage was set, override the image on all nodes
	if opts.NodeImage != "" {
		// Apply image override to all the Nodes defined in Config
		// TODO(fabrizio pandini): this should be reconsidered when implementing
		//     https://github.com/kubernetes-sigs/kind/issues/133
		for i := range opts.Config.Nodes {
			opts.Config.Nodes[i].Image = opts.NodeImage
		}
	}

	// default config fields (important for usage as a library, where the config
	// may be constructed in memory rather than from disk)
	config.SetDefaultsCluster(opts.Config)

	return nil
}
