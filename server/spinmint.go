// Copyright (c) 2017-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/mattermost/mattermost-mattermod/model"
	jenkins "github.com/yosida95/golang-jenkins"

	ltops "github.com/mattermost/mattermost-load-test-ops"
	"github.com/mattermost/mattermost-load-test-ops/terraform"
)

func destroySpinmint(pr *model.PullRequest, instanceId string) {
	LogInfo("Destroying spinmint %v for PR %v in %v/%v", instanceId, pr.Number, pr.RepoOwner, pr.RepoName)

	svc := ec2.New(session.New(), Config.GetAwsConfig())

	params := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{
			&instanceId,
		},
	}

	_, err := svc.TerminateInstances(params)
	if err != nil {
		LogError("Error terminating instances: " + err.Error())
		return
	}
}

func waitForBuildAndSetupLoadtest(pr *model.PullRequest) {
	repo, ok := Config.GetRepository(pr.RepoOwner, pr.RepoName)
	if !ok || repo.JenkinsServer == "" {
		LogError("Unable to set up loadtest for PR %v in %v/%v without Jenkins configured for server", pr.Number, pr.RepoOwner, pr.RepoName)
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to setup loadtest")
		return
	}

	credentials, ok := Config.JenkinsCredentials[repo.JenkinsServer]
	if !ok {
		LogError("No Jenkins credentials for server %v required for PR %v in %v/%v", repo.JenkinsServer, pr.Number, pr.RepoOwner, pr.RepoName)
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to setup loadtest")
		return
	}

	client := jenkins.NewJenkins(&jenkins.Auth{
		Username: credentials.Username,
		ApiToken: credentials.ApiToken,
	}, credentials.URL)

	LogInfo("Waiting for Jenkins to build to set up loadtest for PR %v in %v/%v", pr.Number, pr.RepoOwner, pr.RepoName)

	pr = waitForBuild(client, pr)

	config := &ltops.ClusterConfig{
		Name:                  fmt.Sprintf("pr-%v", pr.Number),
		AppInstanceType:       "m4.xlarge",
		AppInstanceCount:      4,
		DBInstanceType:        "db.r4.xlarge",
		DBInstanceCount:       4,
		LoadtestInstanceCount: 1,
	}
	config.WorkingDirectory = filepath.Join("./clusters/", config.Name)

	LogInfo("Creating terraform cluster for loadtest for PR %v in %v/%v", pr.Number, pr.RepoOwner, pr.RepoName)
	cluster, err := terraform.CreateCluster(config)
	if err != nil {
		LogError("Unable to setup cluster: " + err.Error())
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to setup loadtest")
	}

	results := bytes.NewBuffer(nil)

	LogInfo("Deploying to cluster for loadtest for PR %v in %v/%v", pr.Number, pr.RepoOwner, pr.RepoName)
	if err := cluster.DeployMattermost("https://releases.mattermost.com/mattermost-platform-pr/"+strconv.Itoa(pr.Number)+"/mattermost-enterprise-linux-amd64.tar.gz", "mattermod.mattermost-license"); err != nil {
		LogError("Unable to deploy cluster: " + err.Error())
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to setup loadtest")
		return
	}
	if err := cluster.DeployLoadtests("https://releases.mattermost.com/mattermost-load-test/mattermost-load-test.tar.gz"); err != nil {
		LogError("Unable to deploy loadtests to cluster: " + err.Error())
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to setup loadtest")
		return
	}
	LogInfo("Runing loadtest for PR %v in %v/%v", pr.Number, pr.RepoOwner, pr.RepoName)
	if err := cluster.Loadtest(results); err != nil {
		LogError("Unable to loadtest cluster: " + err.Error())
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to setup loadtest")
		return
	}
	LogInfo("Destroying cluster for PR %v in %v/%v", pr.Number, pr.RepoOwner, pr.RepoName)
	if err := cluster.Destroy(); err != nil {
		LogError("Unable to destroy cluster: " + err.Error())
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, "Failed to setup loadtest")
		return
	}

	commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, results.String())
}

func waitForBuildAndSetupSpinmint(pr *model.PullRequest) {
	repo, ok := Config.GetRepository(pr.RepoOwner, pr.RepoName)
	if !ok || repo.JenkinsServer == "" {
		LogError("Unable to set up spintmint for PR %v in %v/%v without Jenkins configured for server", pr.Number, pr.RepoOwner, pr.RepoName)
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return
	}

	credentials, ok := Config.JenkinsCredentials[repo.JenkinsServer]
	if !ok {
		LogError("No Jenkins credentials for server %v required for PR %v in %v/%v", repo.JenkinsServer, pr.Number, pr.RepoOwner, pr.RepoName)
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return
	}

	client := jenkins.NewJenkins(&jenkins.Auth{
		Username: credentials.Username,
		ApiToken: credentials.ApiToken,
	}, credentials.URL)

	LogInfo("Waiting for Jenkins to build to set up spinmint for PR %v in %v/%v", pr.Number, pr.RepoOwner, pr.RepoName)

	pr = waitForBuild(client, pr)

	instance, err := setupSpinmint(pr.Number, pr.Ref, repo)
	if err != nil {
		LogErrorToMattermost("Unable to set up spinmint for PR %v in %v/%v: %v", pr.Number, pr.RepoOwner, pr.RepoName, err.Error())
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return
	}

	LogInfo("Waiting for instance to come up.")
	time.Sleep(time.Minute * 2)
	publicdns := getPublicDnsName(*instance.InstanceId)

	if err := createRoute53Subdomain(*instance.InstanceId, publicdns); err != nil {
		LogErrorToMattermost("Unable to set up S3 subdomain for PR %v in %v/%v with instance %v: %v", pr.Number, pr.RepoOwner, pr.RepoName, *instance.InstanceId, err.Error())
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintFailedMessage)
		return
	}

	smLink := *instance.InstanceId + ".spinmint.com"
	if Config.SpinmintsUseHttps {
		smLink = "https://" + smLink
	} else {
		smLink = "http://" + smLink
	}

	message := Config.SetupSpinmintDoneMessage
	message = strings.Replace(message, SPINMINT_LINK, smLink, 1)
	message = strings.Replace(message, INSTANCE_ID, INSTANCE_ID_MESSAGE+*instance.InstanceId, 1)

	commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, message)
}

func waitForBuild(client *jenkins.Jenkins, pr *model.PullRequest) *model.PullRequest {
	for {
		if result := <-Srv.Store.PullRequest().Get(pr.RepoOwner, pr.RepoName, pr.Number); result.Err != nil {
			LogError("Unable to get updated PR while waiting for spinmint: %v", result.Err.Error())
		} else {
			// Update the PR in case the build link has changed because of a new commit
			pr = result.Data.(*model.PullRequest)
		}

		if pr.BuildLink != "" {
			LogInfo("BuildLink for %v in %v/%v is %v", pr.Number, pr.RepoOwner, pr.RepoName, pr.BuildLink)
			parts := strings.Split(pr.BuildLink, "/")
			jobNumber, _ := strconv.ParseInt(parts[len(parts)-2], 10, 32)
			jobName := parts[len(parts)-3]

			job, err := client.GetJob(jobName)
			if err != nil {
				LogError("Failed to get Jenkins job %v: %v", jobName, err)
				break
			}

			build, err := client.GetBuild(job, int(jobNumber))
			if err != nil {
				LogErrorToMattermost("Failed to get build %v for PR %v in %v/%v: %v", jobNumber, pr.Number, pr.RepoOwner, pr.RepoName, err)
				break
			}

			if !build.Building && build.Result == "SUCCESS" {
				LogInfo("build %v for PR %v in %v/%v succeeded!", jobNumber, pr.Number, pr.RepoOwner, pr.RepoName)
				break
			} else {
				LogInfo("build %v has status %v %v", jobNumber, build.Result, build.Building)
			}
		} else {
			LogError("Unable to find build link for PR %v", pr.Number)
		}

		time.Sleep(10 * time.Second)
	}
	return pr
}

// Returns instance ID of instance created
func setupSpinmint(prNumber int, prRef string, repo *Repository) (*ec2.Instance, error) {
	LogInfo("Setting up spinmint for PR: " + strconv.Itoa(prNumber))

	svc := ec2.New(session.New(), Config.GetAwsConfig())

	data, err := ioutil.ReadFile(path.Join("config", repo.InstanceSetupScript))
	if err != nil {
		return nil, err
	}
	sdata := string(data)
	sdata = strings.Replace(sdata, "BUILD_NUMBER", strconv.Itoa(prNumber), -1)
	sdata = strings.Replace(sdata, "BRANCH_NAME", prRef, -1)
	bsdata := []byte(sdata)
	sdata = base64.StdEncoding.EncodeToString(bsdata)

	var one int64 = 1
	params := &ec2.RunInstancesInput{
		ImageId:          &Config.AWSImageId,
		MaxCount:         &one,
		MinCount:         &one,
		KeyName:          &Config.AWSKeyName,
		InstanceType:     &Config.AWSInstanceType,
		UserData:         &sdata,
		SecurityGroupIds: []*string{&Config.AWSSecurityGroup},
	}

	resp, err := svc.RunInstances(params)
	if err != nil {
		return nil, err
	}

	return resp.Instances[0], nil
}

func getPublicDnsName(instance string) string {
	svc := ec2.New(session.New(), Config.GetAwsConfig())
	params := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{
			&instance,
		},
	}
	resp, err := svc.DescribeInstances(params)
	if err != nil {
		LogError("Problem getting instance ip: " + err.Error())
		return ""
	}

	return *resp.Reservations[0].Instances[0].PublicDnsName
}

func createRoute53Subdomain(name string, target string) error {
	svc := route53.New(session.New(), Config.GetAwsConfig())

	create := "CREATE"
	var ttl int64 = 30
	cname := "CNAME"
	domainName := fmt.Sprintf("%v.%v", name, "spinmint.com")
	params := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: &create,
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: &domainName,
						TTL:  &ttl,
						Type: &cname,
						ResourceRecords: []*route53.ResourceRecord{
							{
								Value: &target,
							},
						},
					},
				},
			},
		},
		HostedZoneId: &Config.AWSHostedZoneId,
	}

	_, err := svc.ChangeResourceRecordSets(params)
	if err != nil {
		return err
	}

	return nil
}
