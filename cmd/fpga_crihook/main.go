// Copyright 2018 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	utilsexec "k8s.io/utils/exec"
)

const (
	fpgaBitStreamDirectory = "/srv/intel.com/fpga"
	packager               = "/opt/intel/fpga-sw/opae/bin/packager"
	fpgaconf               = "/opt/intel/fpga-sw/opae/fpgaconf-wrapper"
	aocl                   = "/opt/intel/fpga-sw/opencl/aocl-wrapper"
	configJSON             = "config.json"
	fpgaRegionEnvPrefix    = "FPGA_REGION_"
	fpgaAfuEnvPrefix       = "FPGA_AFU_"
	fpgaDevRegexp          = `\/dev\/intel-fpga-port.(\d)$`
	afuIDTemplate          = "/sys/class/fpga/intel-fpga-dev.%s/intel-fpga-port.%s/afu_id"
	interfaceIDTemplate    = "/sys/class/fpga/intel-fpga-dev.%s/intel-fpga-fme.%s/pr/interface_id"
	annotationName         = "com.intel.fpga.mode"
	annotationValue        = "fpga.intel.com/region"
)

var (
	fpgaDevRE = regexp.MustCompile(fpgaDevRegexp)
)

func decodeJSONStream(reader io.Reader) (map[string]interface{}, error) {
	decoder := json.NewDecoder(reader)
	content := make(map[string]interface{})
	err := decoder.Decode(&content)
	return content, errors.WithStack(err)
}

type hookEnv struct {
	bitStreamDir        string
	config              string
	execer              utilsexec.Interface
	afuIDTemplate       string
	interfaceIDTemplate string
}

type fpgaParams struct {
	region string
	afu    string
	devNum string
}

type fpgaBitStream interface {
	validate() error
	program() error
}

type opaeBitStream struct {
	path   string
	params fpgaParams
	execer utilsexec.Interface
}

func (bitStream *opaeBitStream) validate() error {
	region, afu := bitStream.params.region, bitStream.params.afu
	output, err := bitStream.execer.Command(packager, "gbs-info", "--gbs", bitStream.path).CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "%s/%s: can't get bitstream info", region, afu)
	}

	reader := bytes.NewBuffer(output)
	content, err := decodeJSONStream(reader)
	if err != nil {
		return errors.WithMessage(err, fmt.Sprintf("%s/%s: can't decode 'packager gbs-info' output", region, afu))
	}

	afuImage, ok := content["afu-image"]
	if !ok {
		return errors.Errorf("%s/%s: 'afu-image' field not found in the 'packager gbs-info' output", region, afu)
	}

	interfaceUUID, ok := afuImage.(map[string]interface{})["interface-uuid"]
	if !ok {
		return errors.Errorf("%s/%s: 'interface-uuid' field not found in the 'packager gbs-info' output", region, afu)
	}

	acceleratorClusters, ok := afuImage.(map[string]interface{})["accelerator-clusters"]
	if !ok {
		return errors.Errorf("%s/%s: 'accelerator-clusters' field not found in the 'packager gbs-info' output", region, afu)
	}

	if canonize(interfaceUUID.(string)) != region {
		return errors.Errorf("bitstream is not for this device: region(%s) and interface-uuid(%s) don't match", region, interfaceUUID)
	}

	acceleratorTypeUUID, ok := acceleratorClusters.([]interface{})[0].(map[string]interface{})["accelerator-type-uuid"]
	if !ok {
		return errors.Errorf("%s/%s: 'accelerator-type-uuid' field not found in the 'packager gbs-info' output", region, afu)
	}

	if canonize(acceleratorTypeUUID.(string)) != afu {
		return errors.Errorf("incorrect bitstream: AFU(%s) and accelerator-type-uuid(%s) don't match", afu, acceleratorTypeUUID)
	}

	return nil
}

func (bitStream *opaeBitStream) program() error {
	output, err := bitStream.execer.Command(fpgaconf, "-s", bitStream.params.devNum, bitStream.path).CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "failed to program AFU %s to socket %s, region %s: output: %s", bitStream.params.afu, bitStream.params.devNum, bitStream.params.region, string(output))
	}

	return nil
}

type openCLBitStream struct {
	path   string
	params fpgaParams
	execer utilsexec.Interface
}

func (bitStream *openCLBitStream) validate() error {
	return nil
}

func (bitStream *openCLBitStream) program() error {
	output, err := bitStream.execer.Command(aocl, "program", "acl"+bitStream.params.devNum, bitStream.path).CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "failed to program AFU %s to socket %s, region %s: output: %s", bitStream.params.afu, bitStream.params.devNum, bitStream.params.region, string(output))
	}

	return nil
}

func newHookEnv(bitStreamDir string, config string, execer utilsexec.Interface, afuIDTemplate string, interfaceIDTemplate string) *hookEnv {
	return &hookEnv{
		bitStreamDir,
		config,
		execer,
		afuIDTemplate,
		interfaceIDTemplate,
	}
}

func canonize(uuid string) string {
	return strings.ToLower(strings.Replace(uuid, "-", "", -1))
}

func (he *hookEnv) getFPGAParams(content map[string]interface{}) ([]fpgaParams, error) {
	bundle, ok := content["bundle"]
	if !ok {
		return nil, errors.New("no 'bundle' field in the configuration")
	}

	configPath := filepath.Join(fmt.Sprint(bundle), he.config)
	configFile, err := os.Open(configPath)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer configFile.Close()

	content, err = decodeJSONStream(configFile)
	if err != nil {
		return nil, errors.WithMessage(err, "can't decode "+configPath)
	}

	process, ok := content["process"]
	if !ok {
		return nil, errors.Errorf("no 'process' field found in %s", configPath)
	}

	rawEnv, ok := process.(map[string]interface{})["env"]
	if !ok {
		return nil, errors.Errorf("no 'env' field found in the 'process' struct in %s", configPath)
	}

	linux, ok := content["linux"]
	if !ok {
		return nil, errors.Errorf("no 'linux' field found in %s", configPath)
	}

	rawDevices, ok := linux.(map[string]interface{})["devices"]
	if !ok {
		return nil, errors.Errorf("no 'devices' field found in the 'linux' struct in %s", configPath)
	}

	regionEnv := make(map[string]string)
	afuEnv := make(map[string]string)
	envSet := make(map[string]struct{})
	for _, env := range rawEnv.([]interface{}) {
		splitted := strings.SplitN(env.(string), "=", 2)
		if strings.HasPrefix(splitted[0], fpgaRegionEnvPrefix) {
			regionEnv[splitted[0]] = splitted[1]
			underscoreSplitted := strings.SplitN(splitted[0], "_", 3)
			envSet[underscoreSplitted[2]] = struct{}{}
		} else if strings.HasPrefix(splitted[0], fpgaAfuEnvPrefix) {
			afuEnv[splitted[0]] = splitted[1]
			underscoreSplitted := strings.SplitN(splitted[0], "_", 3)
			envSet[underscoreSplitted[2]] = struct{}{}
		}
	}

	devSet := make(map[string]struct{})
	for _, device := range rawDevices.([]interface{}) {
		deviceNum := fpgaDevRE.FindStringSubmatch(device.(map[string]interface{})["path"].(string))
		if deviceNum != nil {
			devSet[deviceNum[1]] = struct{}{}
		}
	}

	return he.produceFPGAParams(regionEnv, afuEnv, envSet, devSet)
}

// produceFPGAParams produce FPGA params from parsed JSON data
func (he *hookEnv) produceFPGAParams(regionEnv, afuEnv map[string]string, envSet, devSet map[string]struct{}) ([]fpgaParams, error) {
	if len(envSet) == 0 {
		return nil, errors.Errorf("No correct FPGA environment variables set")
	}

	if len(envSet) != len(regionEnv) || len(envSet) != len(afuEnv) {
		return nil, errors.Errorf("Environment variables are set incorrectly")
	}

	if len(devSet) != len(envSet) {
		return nil, errors.Errorf("Environment variables don't correspond allocated devices")
	}

	params := []fpgaParams{}
	for bitstreamNum := range envSet {
		interfaceID := canonize(regionEnv[fpgaRegionEnvPrefix+bitstreamNum])
		found := false
		// Find a device suitable for the requested bitstream
		for devNum := range devSet {
			iID, err := he.getInterfaceID(devNum)
			if err != nil {
				return nil, err
			}

			if interfaceID == canonize(iID) {
				params = append(params,
					fpgaParams{
						afu:    canonize(afuEnv[fpgaAfuEnvPrefix+bitstreamNum]),
						region: interfaceID,
						devNum: devNum,
					},
				)
				delete(devSet, devNum)
				found = true
				break
			}
		}
		if !found {
			return nil, errors.Errorf("can't find appropriate device for interfaceID %s", interfaceID)
		}
	}

	return params, nil
}

func (he *hookEnv) getBitStream(params fpgaParams) (fpgaBitStream, error) {
	bitStreamPath := ""
	for _, ext := range []string{".gbs", ".aocx"} {
		bitStreamPath = filepath.Join(he.bitStreamDir, params.region, params.afu+ext)

		_, err := os.Stat(bitStreamPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, errors.Errorf("%s: stat error: %v", bitStreamPath, err)
		}

		if ext == ".gbs" {
			return &opaeBitStream{bitStreamPath, params, he.execer}, nil
		} else if ext == ".aocx" {
			return &openCLBitStream{bitStreamPath, params, he.execer}, nil
		}
	}
	return nil, errors.Errorf("%s/%s: bitstream not found", params.region, params.afu)
}

func (he *hookEnv) getProgrammedAfu(deviceNum string) (string, error) {
	// NOTE: only one region per device is supported, hence
	// deviceNum is used twice (device and port numbers are the same)
	afuIDPath := fmt.Sprintf(he.afuIDTemplate, deviceNum, deviceNum)
	data, err := ioutil.ReadFile(afuIDPath)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (he *hookEnv) getInterfaceID(deviceNum string) (string, error) {
	// NOTE: only one region per device is supported, hence
	// deviceNum is used twice (device and FME numbers are the same)
	interfaceIDPath := fmt.Sprintf(he.interfaceIDTemplate, deviceNum, deviceNum)
	data, err := ioutil.ReadFile(interfaceIDPath)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (he *hookEnv) process(reader io.Reader) error {
	content, err := decodeJSONStream(reader)
	if err != nil {
		return err
	}

	// Check if device plugin annotation is set
	annotations, ok := content["annotations"]
	if !ok {
		return errors.New("no 'annotations' field in the configuration")
	}

	annotation, ok := annotations.(map[string]interface{})[annotationName]
	if !ok {
		fmt.Printf("annotation %s is not set, skipping\n", annotationName)
		return nil
	}
	if annotation != annotationValue {
		fmt.Printf("annotation %s has incorrect value, skipping\n", annotationName)
		return nil
	}

	paramslist, err := he.getFPGAParams(content)
	if err != nil {
		return errors.WithMessage(err, "couldn't get FPGA region, AFU and device number")
	}

	for _, params := range paramslist {
		programmedAfu, err := he.getProgrammedAfu(params.devNum)
		if err != nil {
			return err
		}

		if canonize(programmedAfu) == params.afu {
			// Afu is already programmed
			return nil
		}

		bitStream, err := he.getBitStream(params)
		if err != nil {
			return err
		}

		err = bitStream.validate()
		if err != nil {
			return err
		}

		err = bitStream.program()
		if err != nil {
			return err
		}

		programmedAfu, err = he.getProgrammedAfu(params.devNum)
		if err != nil {
			return err
		}

		if programmedAfu != params.afu {
			return errors.Errorf("programmed function %s instead of %s", programmedAfu, params.afu)
		}
	}

	return nil
}

func main() {
	if os.Getenv("PATH") == "" { // runc doesn't set PATH when runs hooks
		os.Setenv("PATH", "/sbin:/usr/sbin:/usr/local/sbin:/usr/local/bin:/usr/bin:/bin")
	}

	he := newHookEnv(fpgaBitStreamDirectory, configJSON, utilsexec.New(), afuIDTemplate, interfaceIDTemplate)

	err := he.process(os.Stdin)
	if err != nil {
		fmt.Printf("%+v\n", err)
		os.Exit(1)
	}
}
