// Copyright 2020 Intel Corporation. All Rights Reserved.
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
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/klog"
)

const (
	labelNamespace     = "gpu.intel.com/"
	gpuListLabelName   = "cards"
	millicoreLabelName = "millicores"
	millicoresPerGPU   = 1000
	memoryOverrideEnv  = "GPU_MEMORY_OVERRIDE"
	gpuDeviceRE        = `^card[0-9]+$`
	controlDeviceRE    = `^controlD[0-9]+$`
	vendorString       = "0x8086"
)

type labelMap map[string]string

type labeler struct {
	sysfsDir      string
	devfsDir      string
	debugfsDRIDir string

	gpuDeviceReg     *regexp.Regexp
	controlDeviceReg *regexp.Regexp
	labels           labelMap
}

func newLabeler(sysfsDir, devfsDir, debugfsDRIDir string) *labeler {
	return &labeler{
		sysfsDir:         sysfsDir,
		devfsDir:         devfsDir,
		debugfsDRIDir:    debugfsDRIDir,
		gpuDeviceReg:     regexp.MustCompile(gpuDeviceRE),
		controlDeviceReg: regexp.MustCompile(controlDeviceRE),
		labels:           labelMap{},
	}
}

func (l *labeler) scan() ([]string, error) {
	files, err := ioutil.ReadDir(l.sysfsDir)
	gpuNameList := []string{}

	if err != nil {
		return gpuNameList, errors.Wrap(err, "Can't read sysfs folder")
	}

	for _, f := range files {
		if !l.gpuDeviceReg.MatchString(f.Name()) {
			klog.V(4).Info("Not compatible device", f.Name())
			continue
		}

		dat, err := ioutil.ReadFile(path.Join(l.sysfsDir, f.Name(), "device/vendor"))
		if err != nil {
			klog.Warning("Skipping. Can't read vendor file: ", err)
			continue
		}

		if strings.TrimSpace(string(dat)) != vendorString {
			klog.V(4).Info("Non-Intel GPU", f.Name())
			continue
		}

		drmFiles, err := ioutil.ReadDir(path.Join(l.sysfsDir, f.Name(), "device/drm"))
		if err != nil {
			return gpuNameList, errors.Wrap(err, "Can't read device folder")
		}

		for _, drmFile := range drmFiles {
			if l.controlDeviceReg.MatchString(drmFile.Name()) {
				//Skipping possible drm control node
				continue
			}
			devPath := path.Join(l.devfsDir, drmFile.Name())
			if _, err := os.Stat(devPath); err != nil {
				continue
			}

			gpuNameList = append(gpuNameList, f.Name())
			break
		}
	}

	return gpuNameList, nil
}

// getMemoryValues reads the GPU memory amount from the system.
func (l *labeler) getMemoryAmount( /*cardNum*/ string) uint64 {
	// reading GPU local memory amount is not yet available in the driver,
	// so just return the environment variable value
	envValue := os.Getenv(memoryOverrideEnv)
	if envValue != "" {
		val, err := strconv.ParseUint(envValue, 10, 64)
		if err == nil {
			return val
		}
	}
	return 0
}

// addNumericLabel creates a new label if one doesn't exist. Else the new value is added to the previous value.
func (lm labelMap) addNumericLabel(labelName string, valueToAdd int64) {
	value := int64(0)
	if numstr, ok := lm[labelName]; ok {
		_, _ = fmt.Sscanf(numstr, "%d", &value)
	}
	value += valueToAdd
	lm[labelName] = strconv.FormatInt(value, 10)
}

// createCapabilityLabels creates labels from the gpu capability file under debugfs.
func (l *labeler) createCapabilityLabels(cardNum string) {
	// try to read the capabilities from the i915_capabilities file
	file, err := os.Open(filepath.Join(l.debugfsDRIDir, cardNum, "i915_capabilities"))
	if err != nil {
		klog.V(3).Infof("Couldn't open file:%s", err.Error()) // debugfs is not stable, there is no need to spam with error level prints
		return
	}
	defer file.Close()

	// define strings to search from the file, and the actions to take in order to create labels from those strings (as funcs)
	searchStringActionMap := map[string]func(string){
		"platform: ": func(platformName string) {
			l.labels.addNumericLabel(labelNamespace+"platform_"+platformName+".count", 1)
			l.labels[labelNamespace+"platform_"+platformName+".present"] = "true"
		},
		"gen: ": func(genName string) {
			l.labels[labelNamespace+"platform_gen"] = genName
		},
	}

	// Finally, read the file, and try to find the matches. Perform actions and reduce the search map size as we proceed. Return at 0 size.
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		for searchString, action := range searchStringActionMap {
			var stringValue string
			n, _ := fmt.Sscanf(scanner.Text(), searchString+"%s", &stringValue)
			if n > 0 {
				action(stringValue)
				delete(searchStringActionMap, searchString)
				if len(searchStringActionMap) == 0 {
					return
				}
				break
			}
		}
	}
}

// createLabels is the main function of plugin labeler, it creates label-value pairs for the gpus.
func (l *labeler) createLabels() error {
	gpuNameList, err := l.scan()
	if err != nil {
		return err
	}

	for _, gpuName := range gpuNameList {
		gpuNum := ""
		// extract card number as a string. scan() has already checked name syntax
		_, err = fmt.Sscanf(gpuName, "card%s", &gpuNum)
		if err != nil {
			return errors.Wrap(err, "gpu name parsing error")
		}

		// try to add capability labels
		l.createCapabilityLabels(gpuNum)

		// read the memory amount to find a proper max allocation value
		l.labels.addNumericLabel(labelNamespace+"memory.max", int64(l.getMemoryAmount(gpuNum)))
	}
	gpuCount := len(gpuNameList)
	// add gpu list label (example: "card0.card1.card2")
	l.labels[labelNamespace+gpuListLabelName] = strings.Join(gpuNameList, ".")
	// all GPUs get default number of millicores (1000)
	l.labels.addNumericLabel(labelNamespace+millicoreLabelName, int64(millicoresPerGPU*gpuCount))

	return nil
}

func (l *labeler) printLabels() {
	for key, val := range l.labels {
		fmt.Println(key + "=" + val)
	}
}
