// Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/amazon-ecs-cli-v2/internal/pkg/archer"
	"github.com/aws/amazon-ecs-cli-v2/internal/pkg/deploy"
	"github.com/aws/amazon-ecs-cli-v2/internal/pkg/deploy/cloudformation"
	"github.com/aws/amazon-ecs-cli-v2/internal/pkg/manifest"
	"github.com/aws/amazon-ecs-cli-v2/internal/pkg/store/ssm"
	"github.com/aws/amazon-ecs-cli-v2/internal/pkg/term/prompt"
	"github.com/aws/amazon-ecs-cli-v2/internal/pkg/workspace"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	appPackageAppNamePrompt = "Which application would you like to generate a CloudFormation template for?"
	appPackageEnvNamePrompt = "Which environment would you like to create this stack for?"
)

// PackageAppOpts holds the configuration needed to transform an application's manifest to CloudFormation.
type PackageAppOpts struct {
	// Fields with matching flags.
	AppName   string
	EnvName   string
	Tag       string
	OutputDir string

	// Interfaces to interact with dependencies.
	ws             archer.Workspace
	envStore       archer.EnvironmentStore
	templateWriter io.Writer
	paramsWriter   io.Writer
	fs             afero.Fs
	prompt         prompter

	globalOpts // Embed global options.
}

// NewPackageAppOpts returns a new PackageAppOpts where the image tag is set to "manual-{short git sha}".
// The CloudFormation template is written to stdout and the parameters are discarded by default.
// If an error occurred while running git, we set the image name to "latest" instead.
func NewPackageAppOpts() *PackageAppOpts {
	commitID, err := exec.Command("git", "rev-parse", "--short", "HEAD").CombinedOutput()
	if err != nil {
		// If we can't retrieve a commit ID we default the image tag to "latest".
		return &PackageAppOpts{
			Tag:            "latest",
			templateWriter: os.Stdout,
			paramsWriter:   ioutil.Discard,
			fs:             &afero.Afero{Fs: afero.NewOsFs()},
			prompt:         prompt.New(),
			globalOpts:     newGlobalOpts(),
		}
	}
	return &PackageAppOpts{
		Tag:            fmt.Sprintf("manual-%s", strings.TrimSpace(string(commitID))),
		templateWriter: os.Stdout,
		paramsWriter:   ioutil.Discard,
		fs:             &afero.Afero{Fs: afero.NewOsFs()},
		prompt:         prompt.New(),
		globalOpts:     newGlobalOpts(),
	}
}

// Ask prompts the user for any missing required fields.
func (opts *PackageAppOpts) Ask() error {
	if opts.AppName == "" {
		names, err := opts.listAppNames()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return errors.New("there are no applications in the workspace, run `archer init` first")
		}
		app, err := opts.prompt.SelectOne(appPackageAppNamePrompt, "", names)
		if err != nil {
			return fmt.Errorf("prompt application name: %w", err)
		}
		opts.AppName = app
	}
	if opts.EnvName == "" {
		names, err := opts.listEnvNames()
		if err != nil {
			return err
		}
		env, err := opts.prompt.SelectOne(appPackageEnvNamePrompt, "", names)
		if err != nil {
			return fmt.Errorf("prompt environment name: %w", err)
		}
		opts.EnvName = env
	}
	return nil
}

// Validate returns an error if the values provided by the user are invalid.
func (opts *PackageAppOpts) Validate() error {
	if opts.projectName == "" {
		return errNoProjectInWorkspace
	}
	if opts.AppName != "" {
		names, err := opts.listAppNames()
		if err != nil {
			return err
		}
		if !contains(opts.AppName, names) {
			return fmt.Errorf("application '%s' does not exist in the workspace", opts.AppName)
		}
	}
	if opts.EnvName != "" {
		if _, err := opts.envStore.GetEnvironment(opts.projectName, opts.EnvName); err != nil {
			return err
		}
	}
	return nil
}

// Execute prints the CloudFormation template of the application for the environment.
func (opts *PackageAppOpts) Execute() error {
	env, err := opts.envStore.GetEnvironment(opts.projectName, opts.EnvName)
	if err != nil {
		return err
	}

	if opts.OutputDir != "" {
		if err := opts.setFileWriters(); err != nil {
			return err
		}
	}

	tpl, params, err := opts.getTemplates(env)
	if err != nil {
		return err
	}
	if _, err = opts.templateWriter.Write([]byte(tpl)); err != nil {
		return err
	}
	_, err = opts.paramsWriter.Write([]byte(params))
	return err
}

func (opts *PackageAppOpts) listAppNames() ([]string, error) {
	names, err := opts.ws.AppNames()
	if err != nil {
		return nil, fmt.Errorf("list applications in workspace: %w", err)
	}
	return names, nil
}

// getTemplates returns the CloudFormation stack's template and its parameters.
func (opts *PackageAppOpts) getTemplates(env *archer.Environment) (string, string, error) {
	raw, err := opts.ws.ReadManifestFile(opts.ws.ManifestFileName(opts.AppName))
	if err != nil {
		return "", "", err
	}
	mft, err := manifest.UnmarshalApp(raw)
	if err != nil {
		return "", "", err
	}
	switch t := mft.(type) {
	case *manifest.LBFargateManifest:
		stack := cloudformation.NewLBFargateStack(&deploy.CreateLBFargateAppInput{
			App:      mft.(*manifest.LBFargateManifest),
			Env:      env,
			ImageTag: opts.Tag,
		})
		tpl, err := stack.Template()
		if err != nil {
			return "", "", err
		}
		params, err := stack.SerializedParameters()
		return tpl, params, err
	default:
		return "", "", fmt.Errorf("create CloudFormation template for manifest of type %T", t)
	}
}

// setFileWriters creates the output directory, and updates the template and param writers to file writers in the directory.
func (opts *PackageAppOpts) setFileWriters() error {
	if err := opts.fs.MkdirAll(opts.OutputDir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", opts.OutputDir, err)
	}

	templatePath := filepath.Join(opts.OutputDir, fmt.Sprintf("%s.stack.yml", opts.AppName))
	templateFile, err := opts.fs.Create(templatePath)
	if err != nil {
		return fmt.Errorf("create file %s: %w", templatePath, err)
	}
	opts.templateWriter = templateFile

	paramsPath := filepath.Join(opts.OutputDir, fmt.Sprintf("%s-%s.params.json", opts.AppName, opts.EnvName))
	paramsFile, err := opts.fs.Create(paramsPath)
	if err != nil {
		return fmt.Errorf("create file %s: %w", paramsPath, err)
	}
	opts.paramsWriter = paramsFile
	return nil
}

func contains(s string, items []string) bool {
	for _, item := range items {
		if s == item {
			return true
		}
	}
	return false
}

func (opts *PackageAppOpts) listEnvNames() ([]string, error) {
	project := viper.GetString(projectFlag)
	envs, err := opts.envStore.ListEnvironments(project)
	if err != nil {
		return nil, fmt.Errorf("list environments for project %s: %w", project, err)
	}
	var names []string
	for _, env := range envs {
		names = append(names, env.Name)
	}
	return names, nil
}

// BuildAppPackageCmd builds the command for printing an application's CloudFormation template.
func BuildAppPackageCmd() *cobra.Command {
	opts := NewPackageAppOpts()
	cmd := &cobra.Command{
		Use:   "package",
		Short: "Prints the AWS CloudFormation template of an application.",
		Long:  `Prints the CloudFormation template used to deploy an application to an environment.`,
		Example: `
  Print the CloudFormation template for the "frontend" application parametrized for the "test" environment.
  /code $ archer app package -n frontend -e test

  Write the CloudFormation stack and configuration to a "infrastructure/" sub-directory instead of printing.
  /code $ archer app package -n frontend -e test --output-dir ./infrastructure
  /code $ ls ./infrastructure
  /code frontend.stack.yml      frontend-test.config.yml`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			ws, err := workspace.New()
			if err != nil {
				return fmt.Errorf("new workspace: %w", err)
			}
			opts.ws = ws

			store, err := ssm.NewStore()
			if err != nil {
				return fmt.Errorf("couldn't connect to application datastore: %w", err)
			}
			opts.envStore = store
			return opts.Validate()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.Ask(); err != nil {
				return err
			}
			if err := opts.Validate(); err != nil {
				return err
			}
			return opts.Execute()
		},
	}
	// Set the defaults to opts.{Field} otherwise cobra overrides the values set by the constructor.
	cmd.Flags().StringVarP(&opts.AppName, "name", "n", opts.AppName, "Name of the application.")
	cmd.Flags().StringVarP(&opts.EnvName, "env", "e", opts.EnvName, "Name of the environment.")
	cmd.Flags().StringVar(&opts.Tag, "tag", opts.Tag, `Optional. The application's image tag. Defaults to your latest git commit's hash.`)
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "Optional. Writes the stack template and template configuration to a directory.")
	return cmd
}