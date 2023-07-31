package extensions_core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/briandowns/spinner"
	"github.com/skratchdot/open-golang/open"
	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/gql"
	"github.com/superfly/flyctl/internal/appconfig"
	"github.com/superfly/flyctl/internal/flag"
	"github.com/superfly/flyctl/internal/prompt"
	"github.com/superfly/flyctl/iostreams"
	"golang.org/x/exp/slices"
)

type ExtensionOptions struct {
	Provider       string
	SelectName     bool
	SelectRegion   bool
	NameSuffix     string
	DetectPlatform bool
	Options        gql.AddOnOptions
}

type Extension struct {
	Data gql.ExtensionData
	App  gql.AppData
}

var DbExtensionDefaults = ExtensionOptions{
	SelectName:   true,
	SelectRegion: true,
	NameSuffix:   "db",
}

var MonitoringExtensionDefaults = ExtensionOptions{
	Provider:       "sentry",
	SelectName:     false,
	SelectRegion:   false,
	DetectPlatform: true,
}

func ProvisionExtension(ctx context.Context, appName string, options ExtensionOptions) (extension Extension, err error) {
	client := client.FromContext(ctx).API().GenqClient
	io := iostreams.FromContext(ctx)
	colorize := io.ColorScheme()
	// Fetch the target organization from the app
	appResponse, err := gql.GetAppWithAddons(ctx, client, appName, gql.AddOnType(options.Provider))

	if err != nil {
		return
	}

	targetApp := appResponse.App.AppData
	targetOrg := targetApp.Organization
	resp, err := gql.GetAddOnProvider(ctx, client, options.Provider)

	if err != nil {
		return
	}

	addOnProvider := resp.AddOnProvider

	tosResp, err := gql.AgreedToProviderTos(ctx, client, options.Provider, targetOrg.Id)

	if err != nil {
		return
	}

	if !tosResp.Organization.AgreedToProviderTos {
		if err != nil {
			return
		}

		confirmTos, err := prompt.Confirm(ctx, fmt.Sprintf("To continue, your organization must agree to the %s Terms Of Service (%s). Do you agree?", addOnProvider.DisplayName, resp.AddOnProvider.TosUrl))

		if err != nil {
			return extension, err
		}

		if confirmTos {
			_, err := gql.CreateTosAgreement(ctx, client, gql.CreateExtensionTosAgreementInput{
				OrganizationId:    targetOrg.Id,
				AddOnProviderName: options.Provider,
			})

			if err != nil {
				return extension, err
			}
		} else {
			return extension, nil
		}
	}

	if len(appResponse.App.AddOns.Nodes) > 0 {
		errMsg := fmt.Sprintf("A %s extension named %s already exists for this app", addOnProvider.DisplayName, colorize.Green(appResponse.App.AddOns.Nodes[0].Name))
		return extension, errors.New(errMsg)
	}

	var name string

	if options.SelectName {
		name = flag.GetString(ctx, "name")

		if name == "" {
			if options.NameSuffix != "" {
				name = targetApp.Name + "-" + options.NameSuffix
			}
			err = prompt.String(ctx, &name, "Choose a name, use the default, or leave blank to generate one:", name, false)

			if err != nil {
				return
			}
		}
	} else {
		name = targetApp.Name
	}

	input := gql.CreateAddOnInput{
		OrganizationId: targetOrg.Id,
		Name:           name,
		AppId:          targetApp.Id,
		Type:           gql.AddOnType(options.Provider),
		Options:        options.Options,
	}

	if options.SelectRegion {

		var primaryRegion string

		excludedRegions, err := GetExcludedRegions(ctx, options.Provider)

		if err != nil {
			return extension, err
		}

		cfg := appconfig.ConfigFromContext(ctx)

		if cfg != nil && cfg.PrimaryRegion != "" {

			primaryRegion = cfg.PrimaryRegion

			if slices.Contains(excludedRegions, primaryRegion) {
				fmt.Fprintf(io.ErrOut, "%s is only available in regions with low latency (<10ms) to Fly.io regions. That doesn't include '%s'.\n", addOnProvider.DisplayName, primaryRegion)

				confirm, err := prompt.Confirm(ctx, fmt.Sprintf("Would you like to provision anyway in the nearest region to '%s'?", primaryRegion))
				if err != nil || !confirm {
					return extension, err
				}
			}
		} else {

			region, err := prompt.Region(ctx, !targetOrg.PaidPlan, prompt.RegionParams{
				Message:             "Choose the primary region (can't be changed later)",
				ExcludedRegionCodes: excludedRegions,
			})

			if err != nil {
				return extension, err
			}

			primaryRegion = region.Code
		}

		input.PrimaryRegion = primaryRegion
	}

	createResp, err := gql.CreateExtension(ctx, client, input)

	if err != nil {
		return
	}

	extension.Data = createResp.CreateAddOn.AddOn.ExtensionData
	extension.App = targetApp

	if addOnProvider.AsyncProvisioning {
		// wait for provision
		err = WaitForProvision(ctx, extension.Data.Name)
		if err != nil {
			return
		}
	}

	if options.SelectRegion {
		fmt.Fprintf(io.Out, "Created %s in the %s region for app %s\n\n", colorize.Green(extension.Data.Name), colorize.Green(extension.Data.PrimaryRegion), colorize.Green(appName))
	}

	SetSecrets(ctx, &targetApp, extension.Data.Environment.(map[string]interface{}))

	return extension, nil
}

func WaitForProvision(ctx context.Context, name string) error {
	io := iostreams.FromContext(ctx)
	client := client.FromContext(ctx).API().GenqClient

	s := spinner.New(spinner.CharSets[9], 200*time.Millisecond)
	s.Writer = io.ErrOut
	s.Prefix = "Waiting for provisioning to complete "
	s.Start()

	defer s.Stop()
	timeout := time.After(4 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)

	defer ticker.Stop()

	for {

		resp, err := gql.GetAddOn(ctx, client, name)

		if err != nil {
			return err
		}

		if resp.AddOn.Status == "ready" {
			return nil
		}

		select {
		case <-ticker.C:
		case <-timeout:
			return errors.New("timed out waiting for provisioning to complete")
		case <-ctx.Done():
			return nil
		}
	}
}

func GetExcludedRegions(ctx context.Context, provider string) (excludedRegions []string, err error) {
	client := client.FromContext(ctx).API().GenqClient

	response, err := gql.GetAddOnProvider(ctx, client, provider)

	if err != nil {
		return nil, err
	}

	for _, region := range response.AddOnProvider.ExcludedRegions {
		excludedRegions = append(excludedRegions, region.Code)
	}

	return
}

func OpenDashboard(ctx context.Context, extensionName string) (err error) {
	var (
		io     = iostreams.FromContext(ctx)
		client = client.FromContext(ctx).API().GenqClient
	)

	result, err := gql.GetAddOn(ctx, client, extensionName)

	if err != nil {
		return err
	}

	url := result.AddOn.SsoLink
	fmt.Fprintf(io.Out, "Opening %s ...\n", url)

	if err := open.Run(url); err != nil {
		return fmt.Errorf("failed opening %s: %w", url, err)
	}

	return
}

func Discover(ctx context.Context, provider gql.AddOnType) (addOn *gql.AddOnData, app *gql.AppData, err error) {
	client := client.FromContext(ctx).API().GenqClient
	appName := appconfig.NameFromContext(ctx)

	if len(flag.Args(ctx)) == 1 {

		response, err := gql.GetAddOn(ctx, client, flag.FirstArg(ctx))
		if err != nil {
			return nil, nil, err
		}

		addOn = &response.AddOn.AddOnData

	} else if appName != "" {
		resp, err := gql.GetAppWithAddons(ctx, client, appName, provider)

		if err != nil {
			return nil, nil, err
		}

		addOn = &resp.App.AddOns.Nodes[0].AddOnData
		app = &resp.App.AppData
	} else {
		return nil, nil, errors.New("Run this command in a Fly app directory or pass a database name as the first argument.")
	}

	return
}

func SetSecrets(ctx context.Context, app *gql.AppData, secrets map[string]interface{}) error {
	var (
		io     = iostreams.FromContext(ctx)
		client = client.FromContext(ctx).API().GenqClient
	)

	input := gql.SetSecretsInput{
		AppId: app.Id,
	}

	fmt.Fprintf(io.Out, "\nSetting the following secrets on %s:\n", app.Name)

	for key, value := range secrets {
		input.Secrets = append(input.Secrets, gql.SecretInput{Key: key, Value: value.(string)})
		fmt.Println(key)
	}

	fmt.Fprintln(io.Out)

	_, err := gql.SetSecrets(ctx, client, input)

	return err
}
