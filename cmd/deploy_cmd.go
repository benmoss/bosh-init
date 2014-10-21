package cmd

import (
	"errors"
	"fmt"
	"time"

	bosherr "github.com/cloudfoundry/bosh-agent/errors"
	boshlog "github.com/cloudfoundry/bosh-agent/logger"
	boshsys "github.com/cloudfoundry/bosh-agent/system"

	bmagentclient "github.com/cloudfoundry/bosh-micro-cli/agentclient"
	bmconfig "github.com/cloudfoundry/bosh-micro-cli/config"
	bmcpideploy "github.com/cloudfoundry/bosh-micro-cli/cpideployer"
	bmdepl "github.com/cloudfoundry/bosh-micro-cli/deployment"
	bmmicrodeploy "github.com/cloudfoundry/bosh-micro-cli/microdeployer"
	bmretrystrategy "github.com/cloudfoundry/bosh-micro-cli/retrystrategy"
	bmstemcell "github.com/cloudfoundry/bosh-micro-cli/stemcell"
	bmui "github.com/cloudfoundry/bosh-micro-cli/ui"
	bmvalidation "github.com/cloudfoundry/bosh-micro-cli/validation"
)

type deployCmd struct {
	ui                     bmui.UI
	userConfig             bmconfig.UserConfig
	fs                     boshsys.FileSystem
	cpiManifestParser      bmdepl.ManifestParser
	boshManifestParser     bmdepl.ManifestParser
	cpiDeployer            bmcpideploy.CpiDeployer
	stemcellManagerFactory bmstemcell.ManagerFactory
	microDeployer          bmmicrodeploy.Deployer
	deploymentUUID         string
	logger                 boshlog.Logger
	logTag                 string
}

func NewDeployCmd(
	ui bmui.UI,
	userConfig bmconfig.UserConfig,
	fs boshsys.FileSystem,
	cpiManifestParser bmdepl.ManifestParser,
	boshManifestParser bmdepl.ManifestParser,
	cpiDeployer bmcpideploy.CpiDeployer,
	stemcellManagerFactory bmstemcell.ManagerFactory,
	microDeployer bmmicrodeploy.Deployer,
	deploymentUUID string,
	logger boshlog.Logger,
) *deployCmd {
	return &deployCmd{
		ui:                     ui,
		userConfig:             userConfig,
		fs:                     fs,
		cpiManifestParser:      cpiManifestParser,
		boshManifestParser:     boshManifestParser,
		cpiDeployer:            cpiDeployer,
		stemcellManagerFactory: stemcellManagerFactory,
		microDeployer:          microDeployer,
		deploymentUUID:         deploymentUUID,
		logger:                 logger,
		logTag:                 "deployCmd",
	}
}

func (c *deployCmd) Name() string {
	return "deploy"
}

func (c *deployCmd) Run(args []string) error {
	releaseTarballPath, stemcellTarballPath, err := c.validateDeployInputs(args)
	if err != nil {
		return err
	}

	cpiDeployment, err := c.cpiManifestParser.Parse(c.userConfig.DeploymentFile)
	if err != nil {
		return bosherr.WrapError(err, "Parsing CPI deployment manifest `%s'", c.userConfig.DeploymentFile)
	}

	boshDeployment, err := c.boshManifestParser.Parse(c.userConfig.DeploymentFile)
	if err != nil {
		return bosherr.WrapError(err, "Parsing Bosh deployment manifest `%s'", c.userConfig.DeploymentFile)
	}

	cloud, err := c.cpiDeployer.Deploy(cpiDeployment, releaseTarballPath)
	if err != nil {
		return bosherr.WrapError(err, "Deploying CPI `%s'", releaseTarballPath)
	}

	stemcellManager := c.stemcellManagerFactory.NewManager(cloud)
	_, stemcellCID, err := stemcellManager.Upload(stemcellTarballPath)
	if err != nil {
		return bosherr.WrapError(err, "Uploading stemcell from `%s'", stemcellTarballPath)
	}

	agentClient := bmagentclient.NewAgentClient(cpiDeployment.Mbus, c.deploymentUUID, c.logger)
	agentPingRetryable := bmagentclient.NewPingRetryable(agentClient)
	agentPingRetryStrategy := bmretrystrategy.NewAttemptRetryStrategy(300, 500*time.Millisecond, agentPingRetryable, c.logger)
	err = c.microDeployer.Deploy(
		cloud,
		boshDeployment,
		cpiDeployment.Registry,
		cpiDeployment.SSHTunnel,
		agentPingRetryStrategy,
		stemcellCID,
	)
	if err != nil {
		return bosherr.WrapError(err, "Deploying Microbosh")
	}

	// register the stemcell
	return nil
}

type Deployment struct{}

// validateDeployInputs validates the presence of inputs (stemcell tarball, cpi release tarball)
func (c *deployCmd) validateDeployInputs(args []string) (string, string, error) {

	if len(args) != 2 {
		c.ui.Error("Invalid usage - deploy command requires exactly 2 arguments")
		c.ui.Sayln("Expected usage: bosh-micro deploy <cpi-release-tarball> <stemcell-tarball>")
		c.logger.Error(c.logTag, "Invalid arguments: ")
		return "", "", errors.New("Invalid usage - deploy command requires exactly 2 arguments")
	}

	releaseTarballPath := args[0]
	c.logger.Info(c.logTag, "Validating release tarball `%s'", releaseTarballPath)

	fileValidator := bmvalidation.NewFileValidator(c.fs)
	err := fileValidator.Exists(releaseTarballPath)
	if err != nil {
		c.ui.Error(fmt.Sprintf("CPI release `%s' does not exist", releaseTarballPath))
		return "", "", bosherr.WrapError(err, "Checking CPI release `%s' existence", releaseTarballPath)
	}

	stemcellTarballPath := args[1]
	c.logger.Info(c.logTag, "Validating stemcell tarball `%s'", stemcellTarballPath)
	err = fileValidator.Exists(stemcellTarballPath)
	if err != nil {
		c.ui.Error(fmt.Sprintf("Stemcell `%s' does not exist", stemcellTarballPath))
		return "", "", bosherr.WrapError(err, "Checking stemcell `%s' existence", stemcellTarballPath)
	}

	// validate current state: 'microbosh' deployment set
	if len(c.userConfig.DeploymentFile) == 0 {
		c.ui.Error("No deployment set")
		return "", "", bosherr.New("No deployment set")
	}

	c.logger.Info(c.logTag, "Checking for deployment `%s'", c.userConfig.DeploymentFile)
	err = fileValidator.Exists(c.userConfig.DeploymentFile)
	if err != nil {
		c.ui.Error(fmt.Sprintf("Deployment manifest path `%s' does not exist", c.userConfig.DeploymentFile))
		return "", "", bosherr.WrapError(err, "Reading deployment manifest for deploy")
	}

	return releaseTarballPath, stemcellTarballPath, nil
}
