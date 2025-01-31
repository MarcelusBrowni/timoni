/*
Copyright 2023 Stefan Prodan

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

package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"cuelang.org/go/cue/cuecontext"
	"github.com/fluxcd/pkg/ssa"
	"github.com/spf13/cobra"

	apiv1 "github.com/stefanprodan/timoni/api/v1alpha1"
	"github.com/stefanprodan/timoni/internal/engine"
	"github.com/stefanprodan/timoni/internal/runtime"
)

var bundleDelCmd = &cobra.Command{
	Use:     "delete",
	Aliases: []string{"rm", "uninstall"},
	Short:   "Delete all instances from a bundle",
	Long: `The bundle delete command uninstalls the instances and
deletes all their Kubernetes resources from the cluster.'.
`,
	Example: `  # Uninstall all instances in a bundle
  timoni bundle delete -f bundle.cue

  # Uninstall all instances in a named bundle
  timoni bundle delete my-app

  # Uninstall all instances without waiting for finalisation
  timoni bundle delete my-app --wait=false

  # Do a dry-run uninstall and print the changes
  timoni bundle delete my-app --dry-run
`,
	RunE: runBundleDelCmd,
}

type bundleDelFlags struct {
	filename string
	wait     bool
	dryrun   bool
	name     string
}

var bundleDelArgs bundleDelFlags

func init() {
	bundleDelCmd.Flags().BoolVar(&bundleDelArgs.wait, "wait", true,
		"Wait for the deleted Kubernetes objects to be finalized.")
	bundleDelCmd.Flags().BoolVar(&bundleDelArgs.dryrun, "dry-run", false,
		"Perform a server-side delete dry run.")
	bundleDelCmd.Flags().StringVarP(&bundleDelArgs.filename, "file", "f", "",
		"The local path to bundle.cue file.")
	bundleDelCmd.Flags().StringVar(&bundleDelArgs.name, "name", "",
		"Name of the bundle to delete.")
	bundleDelCmd.Flags().MarkDeprecated("name", "use 'timoni bundle delete <name>'")
	bundleCmd.AddCommand(bundleDelCmd)
}

func runBundleDelCmd(cmd *cobra.Command, args []string) error {
	if len(args) < 1 && bundleDelArgs.filename == "" && bundleDelArgs.name == "" {
		return fmt.Errorf("bundle name is required")
	}

	switch {
	case bundleDelArgs.filename != "":
		cuectx := cuecontext.New()
		name, err := engine.ExtractStringFromFile(cuectx, bundleDelArgs.filename, apiv1.BundleName.String())
		if err != nil {
			return err
		}
		bundleDelArgs.name = name
	case len(args) == 1:
		bundleDelArgs.name = args[0]
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), rootArgs.timeout)
	defer cancel()

	sm, err := runtime.NewResourceManager(kubeconfigArgs)
	if err != nil {
		return err
	}

	log := LoggerBundle(ctx, bundleDelArgs.name)
	iStorage := runtime.NewStorageManager(sm)

	instances, err := iStorage.List(ctx, "", bundleDelArgs.name)
	if err != nil {
		return err
	}

	if len(instances) == 0 {
		return fmt.Errorf("no instances found in bundle")
	}

	// delete in revers order (last installed, first to uninstall)
	for index := len(instances) - 1; index >= 0; index-- {
		instance := instances[index]
		log.Info(fmt.Sprintf("deleting instance %s in namespace %s",
			colorizeSubject(instance.Name), colorizeSubject(instance.Namespace)))
		if err := deleteBundleInstance(ctx, &engine.BundleInstance{
			Bundle:    bundleDelArgs.name,
			Name:      instance.Name,
			Namespace: instance.Namespace,
		}, bundleDelArgs.wait, bundleDelArgs.dryrun); err != nil {
			return err
		}
	}
	return nil
}

func deleteBundleInstance(ctx context.Context, instance *engine.BundleInstance, wait bool, dryrun bool) error {
	log := LoggerBundle(ctx, instance.Bundle)

	sm, err := runtime.NewResourceManager(kubeconfigArgs)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), rootArgs.timeout)
	defer cancel()

	iStorage := runtime.NewStorageManager(sm)
	inst, err := iStorage.Get(ctx, instance.Name, instance.Namespace)
	if err != nil {
		return err
	}

	iManager := runtime.InstanceManager{Instance: *inst}
	objects, err := iManager.ListObjects()
	if err != nil {
		return err
	}

	sort.Sort(sort.Reverse(ssa.SortableUnstructureds(objects)))

	if dryrun {
		for _, object := range objects {
			log.Info(colorizeJoin(object, ssa.DeletedAction, dryRunClient))
		}
		return nil
	}

	hasErrors := false
	cs := ssa.NewChangeSet()
	for _, object := range objects {
		deleteOpts := runtime.DeleteOptions(instance.Name, instance.Namespace)
		change, err := sm.Delete(ctx, object, deleteOpts)
		if err != nil {
			log.Error(err, "deletion failed")
			hasErrors = true
			continue
		}
		cs.Add(*change)
		log.Info(colorizeJoin(change))
	}

	if hasErrors {
		os.Exit(1)
	}

	if err := iStorage.Delete(ctx, inst.Name, inst.Namespace); err != nil {
		return err
	}

	deletedObjects := runtime.SelectObjectsFromSet(cs, ssa.DeletedAction)
	if wait && len(deletedObjects) > 0 {
		waitOpts := ssa.DefaultWaitOptions()
		waitOpts.Timeout = rootArgs.timeout
		spin := StartSpinner(fmt.Sprintf("waiting for %v resource(s) to be finalized...", len(deletedObjects)))
		err = sm.WaitForTermination(deletedObjects, waitOpts)
		spin.Stop()
		if err != nil {
			return err
		}
		log.Info("all resources have been deleted")
	}

	return nil
}
