// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/conversion"
	openpeerrors "github.com/openperouter/openperouter/internal/errors"
	"github.com/openperouter/openperouter/internal/grout"
)

// GroutDatapathConfigurator configures the host via the grout DPDK daemon.
type GroutDatapathConfigurator struct {
	groutSocketPath string
}

func newGroutConfigurator(groutSocketPath string) *GroutDatapathConfigurator {
	return &GroutDatapathConfigurator{
		groutSocketPath: groutSocketPath,
	}
}

func (g *GroutDatapathConfigurator) Validate(apiConfig conversion.APIConfigData) error {
	resourceErrors := make([]error, 0, 1)

	for _, l3Passthrough := range apiConfig.L3Passthrough {
		resourceErrors = append(resourceErrors, conversion.ValidateGroutL3Passthrough(l3Passthrough))
	}
	for _, l3VNI := range apiConfig.L3VNIs {
		resourceErrors = append(resourceErrors, conversion.ValidateGroutL3VNI(l3VNI))
	}
	for _, l2VNI := range apiConfig.L2VNIs {
		resourceErrors = append(resourceErrors, conversion.ValidateGroutL2VNI(l2VNI))
	}
	for _, underlay := range apiConfig.Underlays {
		resourceErrors = append(resourceErrors, conversion.ValidateGroutUnderlay(underlay))
	}
	return errors.Join(resourceErrors...)
}

func (g *GroutDatapathConfigurator) Configure(ctx context.Context, config interfacesConfiguration) error {
	groutClient := grout.NewClient(g.groutSocketPath)

	hasAlreadyUnderlay, err := grout.HasUnderlayInterface(ctx, groutClient)
	if err != nil {
		return fmt.Errorf("failed to check if target namespace %s has underlay: %w", config.targetNamespace, err)
	}
	if hasAlreadyUnderlay && len(config.Underlays) == 0 {
		slog.InfoContext(ctx, "underlay removed, cleaning up grout underlay")
		if err := grout.RemoveUnderlay(ctx, groutClient, config.targetNamespace); err != nil {
			slog.Warn("failed to remove underlay after underlay removal", "err", err)
		}
		return nil
	}

	if len(config.Underlays) == 0 {
		return nil // nothing to do
	}

	slog.InfoContext(ctx, "configure interface start", "namespace", config.targetNamespace)
	defer slog.InfoContext(ctx, "configure interface end", "namespace", config.targetNamespace)
	apiConfig := conversion.APIConfigData{
		Underlays:     config.Underlays,
		L3VNIs:        config.L3VNIs,
		L2VNIs:        config.L2VNIs,
		L3Passthrough: config.L3Passthrough,
	}
	hostConfig, err := conversion.APItoHostConfig(config.nodeIndex, config.targetNamespace, apiConfig)
	if err != nil {
		return fmt.Errorf("failed to convert config to host configuration: %w", err)
	}

	slog.InfoContext(ctx, "setting up underlay")
	if err := grout.SetupUnderlay(ctx, groutClient, hostConfig.Underlay); err != nil {
		return fmt.Errorf("failed to setup underlay: %w", err)
	}

	var resourceErrors []error
	reason := v1alpha1.FailedResourceReasonOverlayAttachmentFailed

	slog.InfoContext(ctx, "setting up passthrough")
	if hostConfig.L3Passthrough != nil {
		if err := grout.SetupPassthrough(ctx, groutClient, *hostConfig.L3Passthrough); err != nil {
			resourceErrors = append(resourceErrors, &openpeerrors.ResourceError{
				Obj: v1alpha1.FailedResource{
					Kind: openpeerrors.KindL3Passthrough, Name: config.L3Passthrough[0].Name, Reason: reason, Message: err.Error(),
				},
			})
		}
	}

	if len(apiConfig.L3Passthrough) == 0 {
		if err := grout.RemovePassthrough(ctx, groutClient); err != nil {
			return fmt.Errorf("failed to remove passthrough: %w", err)
		}
	}

	return errors.Join(resourceErrors...)
}
