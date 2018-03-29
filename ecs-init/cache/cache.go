// Copyright 2015 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package cache provides functionality for working with an on-disk cache of
// the ECS Agent image.
package cache

import (
	"bufio"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/aws/amazon-ecs-init/ecs-init/config"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	log "github.com/cihub/seelog"
)

const (
	orwPerm = 0700
)

// Downloader is responsible for cache operations relating to downloading the agent
type Downloader struct {
	getter   httpGetter
	fs       fileSystem
	metadata instanceMetadata
	region   string
}

// NewDownloader returns a Downloader with default dependencies
func NewDownloader() *Downloader {
	downloader := &Downloader{
		getter: customGetter,
		fs:     &standardFS{},
	}

	// If metadata cannot be initialized the region string is populated with the default value to prevent future
	// calls to retrieve the region from metadata
	sessionInstance, err := session.NewSession()
	if err != nil {
		downloader.region = config.DefaultRegionName
		return downloader
	}
	downloader.metadata = ec2metadata.New(sessionInstance)
	return downloader
}

// IsAgentCached returns true if there is a cached copy of the Agent present
// and a cache state file is not empty (no validation is performed on the
// tarball or cache state file contents)
func (d *Downloader) IsAgentCached() bool {
	return d.fileNotEmpty(config.CacheState()) && d.fileNotEmpty(config.AgentTarball())
}

func (d *Downloader) fileNotEmpty(filename string) bool {
	fileinfo, err := d.fs.Stat(filename)
	if err != nil {
		return false
	}
	return fileinfo.Size() > 0
}

// getRegion finds region from metadata and caches for the life of downloader
func (d *Downloader) getRegion() string {
	if d.region != "" {
		return d.region
	}

	region, err := d.metadata.Region()
	if err != nil {
		log.Warn("Could not retrieve the region from EC2 Instance Metadata. Error: %s", err.Error())
		region = config.DefaultRegionName
	}
	d.region = region

	return d.region
}

// DownloadAgent downloads a fresh copy of the Agent and performs an
// integrity check on the downloaded image
func (d *Downloader) DownloadAgent() error {
	err := d.fs.MkdirAll(config.CacheDirectory(), os.ModeDir|orwPerm)
	if err != nil {
		return err
	}

	publishedMd5Sum, err := d.getPublishedMd5Sum()
	if err != nil {
		return err
	}

	publishedTarballReader, err := d.getPublishedTarball()
	if err != nil {
		return err
	}
	defer publishedTarballReader.Close()

	md5hash := md5.New()
	tempFile, err := d.fs.TempFile(config.CacheDirectory(), "ecs-agent.tar")
	if err != nil {
		return err
	}
	log.Debugf("Temp file %s", tempFile.Name())
	defer func() {
		if err != nil {
			log.Debugf("Removing temp file %s", tempFile.Name())
			d.fs.Remove(tempFile.Name())
		}
	}()
	defer tempFile.Close()

	teeReader := d.fs.TeeReader(publishedTarballReader, md5hash)
	_, err = d.fs.Copy(tempFile, teeReader)
	if err != nil {
		return err
	}

	calculatedMd5Sum := md5hash.Sum(nil)
	calculatedMd5SumString := fmt.Sprintf("%x", calculatedMd5Sum)
	log.Debugf("Expected %s", publishedMd5Sum)
	log.Debugf("Calculated %s", calculatedMd5SumString)
	agentRemoteTarball := config.AgentRemoteTarball(d.getRegion())
	if publishedMd5Sum != calculatedMd5SumString {
		err = fmt.Errorf("mismatched md5sum while downloading %s", agentRemoteTarball)
		return err
	}

	log.Debugf("Attempting to rename %s to %s", tempFile.Name(), config.AgentTarball())
	return d.fs.Rename(tempFile.Name(), config.AgentTarball())
}

func (d *Downloader) getPublishedMd5Sum() (string, error) {
	region := d.getRegion()
	agentRemoteTarballMD5 := config.AgentRemoteTarballMD5(region)
	log.Debugf("Downloading published md5sum from %s", agentRemoteTarballMD5)
	resp, err := d.getter.Get(agentRemoteTarballMD5)
	if err != nil {
		return "", err
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()
	body, err := d.fs.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func (d *Downloader) getPublishedTarball() (io.ReadCloser, error) {
	region := d.getRegion()
	agentRemoteTarball := config.AgentRemoteTarball(region)
	log.Debugf("Downloading Amazon Elastic Container Service Agent from %s", agentRemoteTarball)
	resp, err := d.getter.Get(agentRemoteTarball)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected response code %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// LoadCachedAgent returns an io.ReadCloser of the Agent from the cache
func (d *Downloader) LoadCachedAgent() (io.ReadCloser, error) {
	return d.fs.Open(config.AgentTarball())
}

func (d *Downloader) RecordCachedAgent() error {
	data := []byte("1")
	return d.fs.WriteFile(config.CacheState(), data, orwPerm)
}

// LoadDesiredAgent returns an io.ReadCloser of the Agent indicated by the desiredImageLocatorFile
// (/var/cache/ecs/desired-image). The desiredImageLocatorFile must contain as the beginning of the file the name of
// the file containing the desired image (interpreted as a basename) and ending in a newline.  Only the first line is
// read, with the rest of the file reserved for future use.
func (d *Downloader) LoadDesiredAgent() (io.ReadCloser, error) {
	desiredImageFile, err := d.getDesiredImageFile()
	if err != nil {
		return nil, err
	}
	return d.fs.Open(desiredImageFile)
}

func (d *Downloader) getDesiredImageFile() (string, error) {
	file, err := d.fs.Open(config.DesiredImageLocatorFile())
	if err != nil {
		return "", err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	desiredImageString, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	desiredImageFile := strings.TrimSpace(config.CacheDirectory() + "/" + d.fs.Base(desiredImageString))
	return desiredImageFile, nil
}
