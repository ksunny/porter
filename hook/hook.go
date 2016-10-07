/*
 *  Copyright 2016 Adobe Systems Incorporated. All rights reserved.
 *  This file is licensed to you under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License. You may obtain a copy
 *  of the License at http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software distributed under
 *  the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR REPRESENTATIONS
 *  OF ANY KIND, either express or implied. See the License for the specific language
 *  governing permissions and limitations under the License.
 */
package hook

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/adobe-platform/porter/aws/elb"
	"github.com/adobe-platform/porter/aws_session"
	"github.com/adobe-platform/porter/conf"
	"github.com/adobe-platform/porter/constants"
	"github.com/adobe-platform/porter/logger"
	"github.com/adobe-platform/porter/provision_output"
	"github.com/inconshreveable/log15"
)

type (
	regionHookRunner struct {
		logOutput bytes.Buffer

		runOutput *chan bytes.Buffer

		serviceName string
		hookName    string

		commandSuccess bool
	}

	hookLinkedList struct {
		hook      conf.Hook
		hookIndex int

		next *hookLinkedList
	}
)

var (
	// Multi-region deployment means we need a globally unique id for git clones
	// and image names
	globalCounter *uint32 = new(uint32)

	logMutex sync.Mutex
)

func Execute(log log15.Logger,
	hookName, environment string,
	provisionedRegions []provision_output.Region,
	commandSuccess bool) bool {

	return ExecuteWithRunCapture(log, hookName, environment, provisionedRegions,
		commandSuccess, nil)
}

func ExecuteWithRunCapture(log log15.Logger,
	hookName, environment string,
	provisionedRegions []provision_output.Region,
	commandSuccess bool, runOutput *chan bytes.Buffer) (success bool) {

	var err error

	log = log.New("HookName", hookName)
	log.Info("Hook BEGIN")
	defer log.Info("Hook END")

	config, configSuccess := conf.GetConfig(log, false)
	if !configSuccess {
		return
	}

	workingDir, err := os.Getwd()
	if err != nil {
		log.Error("Getwd", "Error", err)
		return
	}
	log.Debug("os.Getwd()", "Path", workingDir)

	var configHooks []conf.Hook

	configHooks, exists := config.Hooks[hookName]
	if !exists {
		success = true
		return
	}

	if environment == "" {

		runArgs := runArgsFactory(log, config, workingDir)

		hookRunner := &regionHookRunner{

			runOutput: runOutput,

			serviceName: config.ServiceName,
			hookName:    hookName,

			commandSuccess: commandSuccess,
		}

		success = hookRunner.runConfigHooks(log, configHooks, runArgs)

	} else {

		env, err := config.GetEnvironment(environment)
		if err != nil {
			log.Error("GetEnvironment", "Error", err)
			return
		}

		// This applies to pre-provision which doesn't have provisioning output
		// since the provision command hasn't run but should work multi-region
		// in the same way that post-provision and others behave
		if provisionedRegions == nil {
			provisionedRegions = make([]provision_output.Region, 0)

			for _, region := range env.Regions {
				pr := provision_output.Region{
					AWSRegion: region.Name,
				}
				provisionedRegions = append(provisionedRegions, pr)
			}
		}

		successChan := make(chan bool)

		for _, pr := range provisionedRegions {

			elbDNS := ""

			region, err := env.GetRegion(pr.AWSRegion)
			if err != nil {
				log.Error("GetRegion", "Error", err)
				return
			}

			roleARN, err := env.GetRoleARN(region.Name)
			if err != nil {
				log.Error("GetRoleARN", "Error", err)
				return
			}

			roleSession := aws_session.STS(region.Name, roleARN, 3600)

			if pr.ProvisionedELBName != "" {
				elbClient := elb.New(roleSession)

				log.Info("DescribeLoadBalancers")
				output, err := elb.DescribeLoadBalancers(elbClient, pr.ProvisionedELBName)
				if err != nil {
					log.Error("DescribeLoadBalancers", "Error", err)
					return
				}
				if len(output) == 1 {
					elbDNS = *output[0].DNSName
				} else {
					log.Warn("DescribeLoadBalancers - no ELB found")
				}
			}

			credValue, err := roleSession.Config.Credentials.Get()
			if err != nil {
				log.Warn("Couldn't get AWS credential values. Hooks calling AWS APIs will fail")
			}

			runArgs := runArgsFactory(log, config, workingDir)

			runArgs = append(runArgs,
				"-e", "PORTER_ENVIRONMENT="+environment,
				"-e", "AWS_REGION="+region.Name,
				// AWS_DEFAULT_REGION is also needed for AWS SDKs
				"-e", "AWS_DEFAULT_REGION="+region.Name,
				"-e", "AWS_ACCESS_KEY_ID="+credValue.AccessKeyID,
				"-e", "AWS_SECRET_ACCESS_KEY="+credValue.SecretAccessKey,
				"-e", "AWS_SESSION_TOKEN="+credValue.SessionToken,
				"-e", "AWS_SECURITY_TOKEN="+credValue.SessionToken,
			)

			if elbDNS != "" {
				runArgs = append(runArgs,
					"-e", "AWS_ELASTICLOADBALANCING_LOADBALANCER_DNS="+elbDNS)
			}

			if pr.StackId != "" {
				runArgs = append(runArgs,
					"-e", "AWS_CLOUDFORMATION_STACKID="+pr.StackId)
			}

			hookRunner := &regionHookRunner{
				runOutput: runOutput,

				serviceName: config.ServiceName,
				hookName:    hookName,

				commandSuccess: commandSuccess,
			}

			go func(runner *regionHookRunner, log log15.Logger,
				hooks []conf.Hook, runArgs []string) {

				log = log.New()
				logger.SetHandler(log, &runner.logOutput)

				successChan <- runner.runConfigHooks(log, hooks, runArgs)

				logMutex.Lock()
				runner.logOutput.WriteTo(os.Stdout)
				logMutex.Unlock()

			}(hookRunner, log, configHooks, runArgs)
		}

		success = true

		for i := 0; i < len(provisionedRegions); i++ {

			runConfigHooksSuccess := <-successChan

			success = success && runConfigHooksSuccess
		}
	}

	return
}

func runArgsFactory(log log15.Logger, config *conf.Config, workingDir string) []string {
	runArgs := []string{
		"run",
		"--rm",
		"-v", fmt.Sprintf("%s:%s", workingDir, "/repo_root"),
		"-e", "PORTER_SERVICE_NAME=" + config.ServiceName,
		"-e", "DOCKER_ENV_FILE=" + constants.EnvFile,
		"-e", "HAPROXY_STATS_USERNAME=" + constants.HAProxyStatsUsername,
		"-e", "HAPROXY_STATS_PASSWORD=" + constants.HAProxyStatsPassword,
		"-e", "HAPROXY_STATS_URL=" + constants.HAProxyStatsUrl,
	}

	revParseOutput, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err == nil {
		sha1 := strings.TrimSpace(string(revParseOutput))

		runArgs = append(runArgs, "-e", "PORTER_SERVICE_VERSION="+sha1)
	}

	var warnedDeprecation bool
	for _, kvp := range os.Environ() {
		if strings.HasPrefix(kvp, "PORTER_") {
			if !warnedDeprecation {
				warnedDeprecation = true
				log.Warn("Hook environments configured with PORTER_ is deprecated. In future releases and this will be an error http://bit.ly/2ar6fcQ")
			}
			log.Debug("Deprecated environment", "Env", kvp)
			runArgs = append(runArgs, "-e", strings.TrimPrefix(kvp, "PORTER_"))
		}
	}

	return runArgs
}

func (recv *regionHookRunner) runConfigHooks(log log15.Logger, hooks []conf.Hook, runArgs []string) (success bool) {

	successChan := make(chan bool)
	var (
		concurrentCount   int
		eligibleHookCount int

		head *hookLinkedList
		tail *hookLinkedList
	)

	// Only retained lexical scope is successChan, everything else is copied
	goGadgetHook := func(log log15.Logger, hookIndex int, hook conf.Hook, runArgs []string) {

		successChan <- recv.runConfigHook(log, hookIndex, hook, runArgs)
	}

	for hookIndex, hook := range hooks {
		if recv.commandSuccess {
			if hook.RunCondition == constants.HRC_Fail {
				continue
			}
		} else {
			if hook.RunCondition == constants.HRC_Pass {
				continue
			}
		}
		eligibleHookCount++

		next := &hookLinkedList{
			hook:      hook,
			hookIndex: hookIndex,
		}

		if head == nil {
			head = next
		}

		if tail == nil {
			tail = head
		} else {
			tail.next = next
			tail = next
		}
	}

	if recv.runOutput != nil {
		*recv.runOutput = make(chan bytes.Buffer, eligibleHookCount)
	}

	for node := head; node != nil; node = node.next {

		if node.hook.Concurrent {

			concurrentCount++
		} else {

			concurrentCount = 1
		}

		log := log.New(
			"HookIndex", node.hookIndex,
			"Concurrent", node.hook.Concurrent,
			"Repo", node.hook.Repo,
			"Ref", node.hook.Ref,
			"RunCondition", node.hook.RunCondition,
		)

		log.Debug("go go gadget hook")
		go goGadgetHook(log, node.hookIndex, node.hook, runArgs)

		// block anytime we're not running consecutive concurrent hooks
		if node.next == nil || !node.next.hook.Concurrent {

			log.Debug("Waiting for hook(s) to finish", "concurrentCount", concurrentCount)
			for i := 0; i < concurrentCount; i++ {
				success = <-successChan
				if !success {
					return
				}
			}
			log.Debug("Hook(s) finished", "concurrentCount", concurrentCount)

			concurrentCount = 0
		}
	}

	success = true
	return
}

func (recv *regionHookRunner) runConfigHook(log log15.Logger, hookIndex int,
	hook conf.Hook, runArgs []string) (success bool) {

	log.Debug("runConfigHook() BEGIN")
	defer log.Debug("runConfigHook() END")

	for envKey, envValue := range hook.Environment {
		if envValue == "" {
			envValue = os.Getenv(envKey)
		}
		runArgs = append(runArgs, "-e", envKey+"="+envValue)
		log.Debug("Configured environment", "Key", envKey, "Value", envValue)
	}

	dockerFilePath := hook.Dockerfile
	hookCounter := atomic.AddUint32(globalCounter, 1)

	if hook.Repo != "" {

		repoDir := fmt.Sprintf("%s_clone_%d_%d", recv.hookName, hookIndex, hookCounter)
		repoDir = path.Join(constants.TempDir, repoDir)

		defer exec.Command("rm", "-fr", repoDir).Run()

		log.Info("git clone",
			"Repo", hook.Repo,
			"Ref", hook.Ref,
			"Directory", repoDir,
		)

		cloneCmd := exec.Command(
			"git", "clone",
			"--branch", hook.Ref, // this works on tags as well
			"--depth", "1",
			hook.Repo, repoDir,
		)
		cloneCmd.Stdout = &recv.logOutput
		cloneCmd.Stderr = &recv.logOutput
		err := cloneCmd.Run()
		if err != nil {
			log.Error("git clone", "Error", err)
			return
		}

		// Validation ensures that local hooks have a dockerfile path
		// Plugins default to Dockerfile
		if dockerFilePath == "" {
			dockerFilePath = "Dockerfile"
		}

		dockerFilePath = path.Join(repoDir, dockerFilePath)
	}

	imageName := fmt.Sprintf("%s-%s-%d-%d",
		recv.serviceName, recv.hookName, hookIndex, hookCounter)

	if !recv.buildAndRun(log, imageName, dockerFilePath, runArgs) {
		return
	}

	success = true
	return
}

func (recv *regionHookRunner) buildAndRun(log log15.Logger, imageName,
	dockerFilePath string, runArgs []string) (success bool) {

	log = log.New("Dockerfile", dockerFilePath, "ImageName", imageName)

	log.Debug("buildAndRun() BEGIN")
	defer log.Debug("buildAndRun() END")

	var logOutput bytes.Buffer
	var runOutput bytes.Buffer

	defer func() {

		if recv.runOutput != nil {
			*recv.runOutput <- runOutput
		}

		logOutput.WriteTo(&recv.logOutput)
		runOutput.WriteTo(&recv.logOutput)
	}()

	log.Info("You are now exiting porter and entering a porter deployment hook")
	log.Info("Deployment hooks are used to hook into porter's build lifecycle")
	log.Info("None of the code about to be run is porter code")
	log.Info("If you experience problems talk to the author of this Dockerfile")
	log.Info("You can read more about deployment hooks here http://bit.ly/2dKBwd0")

	dockerBuildCmd := exec.Command("docker", "build",
		"-t", imageName,
		"-f", dockerFilePath,
		path.Dir(dockerFilePath),
	)
	dockerBuildCmd.Stdout = &logOutput
	dockerBuildCmd.Stderr = &logOutput

	log.Info("Building deployment hook START")
	log.Info("==============================")
	err := dockerBuildCmd.Run()
	log.Info("============================")
	log.Info("Building deployment hook END")

	if err != nil {
		log.Error("docker build", "Error", err)
		log.Info("This is not a problem with porter but with the Dockerfile porter tried to build")
		log.Info("DO NOT contact Brandon Cook to help debug this issue")
		log.Info("DO NOT file an issue against porter")
		log.Info("DO contact the author of the Dockerfile or try to reproduce the problem by running docker build on this machine")
		return
	}

	runArgs = append(runArgs, imageName)

	log.Debug("docker run", "Args", runArgs)

	runCmd := exec.Command("docker", runArgs...)
	runCmd.Stdout = &runOutput
	runCmd.Stderr = &logOutput

	log.Info("Running deployment hook START")
	log.Info("==============================")
	err = runCmd.Run()
	log.Info("===========================")
	log.Info("Running deployment hook END")

	if err != nil {
		log.Error("docker run", "Error", err)
		log.Info("This is not a problem with porter but with the Dockerfile porter tried to run")
		log.Info("DO NOT contact Brandon Cook to help debug this issue")
		log.Info("DO NOT file an issue against porter")
		log.Info("DO contact the author of the Dockerfile")
		log.Info("Run `porter help debug` to see how to enable debug logging which will show you the arguments used in docker run")
		log.Info("Be aware that enabling debug logging will print sensitive data including, but not limited to, AWS credentials")
		return
	}

	success = true
	return
}
