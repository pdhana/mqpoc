/*
Package mqmetric contains a set of routines common to several
commands used to export MQ metrics to different backend
storage mechanisms including Prometheus and InfluxDB.
*/
package mqmetric

/*
  Copyright (c) IBM Corporation 2016, 2019

  Licensed under the Apache License, Version 2.0 (the "License");
  you may not use this file except in compliance with the License.
  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

  Unless required by applicable law or agreed to in writing, software
  distributed under the License is distributed on an "AS IS" BASIS,
  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  See the License for the specific language governing permissions and
  limitations under the License.

   Contributors:
     Mark Taylor - Initial Contribution
*/

/*
Functions in this file use the DISPLAY USAGE    command to extract metrics
about MQ on z/OS pageset and bufferpool use.
*/

import (
	//	"fmt"
	"github.com/ibm-messaging/mq-golang/v5/ibmmq"
	"strconv"
)

const (
	ATTR_BP_ID           = "id"
	ATTR_BP_LOCATION     = "location"
	ATTR_BP_CLASS        = "pageclass"
	ATTR_BP_FREE         = "buffers_free"
	ATTR_BP_FREE_PERCENT = "buffers_free_percent"
	ATTR_BP_TOTAL        = "buffers_total"

	ATTR_PS_ID           = "id"
	ATTR_PS_BPID         = "bufferpool"
	ATTR_PS_TOTAL        = "pages_total"
	ATTR_PS_UNUSED       = "pages_unused"
	ATTR_PS_NP_PAGES     = "pages_nonpersistent"
	ATTR_PS_P_PAGES      = "pages_persistent"
	ATTR_PS_STATUS       = "status"
	ATTR_PS_EXPAND_COUNT = "expansion_count"
)

var UsageBpStatus StatusSet
var UsagePsStatus StatusSet
var usageAttrsInit = false

func UsageInitAttributes() {
	if usageAttrsInit {
		return
	}
	UsageBpStatus.Attributes = make(map[string]*StatusAttribute)
	UsagePsStatus.Attributes = make(map[string]*StatusAttribute)

	attr := ATTR_BP_ID
	UsageBpStatus.Attributes[attr] = newPseudoStatusAttribute(attr, "Buffer Pool ID")
	attr = ATTR_BP_LOCATION
	UsageBpStatus.Attributes[attr] = newPseudoStatusAttribute(attr, "Buffer Pool Location")
	attr = ATTR_BP_CLASS
	UsageBpStatus.Attributes[attr] = newPseudoStatusAttribute(attr, "Buffer Pool Class")

	// These are the integer status fields that are of interest
	attr = ATTR_BP_FREE
	UsageBpStatus.Attributes[attr] = newStatusAttribute(attr, "Free buffers", ibmmq.MQIACF_USAGE_FREE_BUFF)
	attr = ATTR_BP_FREE_PERCENT
	UsageBpStatus.Attributes[attr] = newStatusAttribute(attr, "Free buffers percent", ibmmq.MQIACF_USAGE_FREE_BUFF_PERC)
	attr = ATTR_BP_TOTAL
	UsageBpStatus.Attributes[attr] = newStatusAttribute(attr, "Total buffers", ibmmq.MQIACF_USAGE_TOTAL_BUFFERS)

	attr = ATTR_PS_ID
	UsagePsStatus.Attributes[attr] = newPseudoStatusAttribute(attr, "Pageset ID")
	attr = ATTR_PS_BPID
	UsagePsStatus.Attributes[attr] = newPseudoStatusAttribute(attr, "Buffer Pool ID")
	attr = ATTR_PS_TOTAL
	UsagePsStatus.Attributes[attr] = newStatusAttribute(attr, "Total pages", ibmmq.MQIACF_USAGE_TOTAL_PAGES)
	attr = ATTR_PS_UNUSED
	UsagePsStatus.Attributes[attr] = newStatusAttribute(attr, "Unused pages", ibmmq.MQIACF_USAGE_UNUSED_PAGES)
	attr = ATTR_PS_NP_PAGES
	UsagePsStatus.Attributes[attr] = newStatusAttribute(attr, "Non-persistent pages", ibmmq.MQIACF_USAGE_NONPERSIST_PAGES)
	attr = ATTR_PS_P_PAGES
	UsagePsStatus.Attributes[attr] = newStatusAttribute(attr, "Persistent pages", ibmmq.MQIACF_USAGE_PERSIST_PAGES)
	attr = ATTR_PS_STATUS
	UsagePsStatus.Attributes[attr] = newStatusAttribute(attr, "Status", ibmmq.MQIACF_PAGESET_STATUS)
	attr = ATTR_PS_EXPAND_COUNT
	UsagePsStatus.Attributes[attr] = newStatusAttribute(attr, "Expansion Count", ibmmq.MQIACF_USAGE_EXPAND_COUNT)

	usageAttrsInit = true
}

func CollectUsageStatus() error {
	var err error

	UsageInitAttributes()

	// Empty any collected values
	for k := range UsageBpStatus.Attributes {
		UsageBpStatus.Attributes[k].Values = make(map[string]*StatusValue)
	}
	for k := range UsagePsStatus.Attributes {
		UsagePsStatus.Attributes[k].Values = make(map[string]*StatusValue)
	}
	err = collectUsageStatus()

	return err
}

func collectUsageStatus() error {
	var err error
	statusClearReplyQ()

	putmqmd, pmo, cfh, buf := statusSetCommandHeaders()
	// Can allow all the other fields to default
	cfh.Command = ibmmq.MQCMD_INQUIRE_USAGE

	// There are no additional parameters required as the
	// default behaviour of the command returns what we need

	// Once we know the total number of parameters, put the
	// CFH header on the front of the buffer.
	buf = append(cfh.Bytes(), buf...)

	// And now put the command to the queue
	err = cmdQObj.Put(putmqmd, pmo, buf)
	if err != nil {
		return err

	}

	for allReceived := false; !allReceived; {
		cfh, buf, allReceived, err = statusGetReply()
		if buf != nil {
			//	fmt.Printf("UsageBP Data received. cfh %v err %v\n",cfh,err)
			parseUsageData(cfh, buf)
		}

	}

	return err
}

// Given a PCF response message, parse it to extract the desired statistics
func parseUsageData(cfh *ibmmq.MQCFH, buf []byte) string {
	var elem *ibmmq.PCFParameter
	var responseType int32
	bpId := ""
	bpLocation := ""
	bpClass := ""
	psId := ""

	key := ""
	parmAvail := true
	bytesRead := 0
	offset := 0
	datalen := len(buf)
	if cfh == nil || cfh.ParameterCount == 0 {
		return ""
	}

	// Parse it once to extract the fields that are needed for the map key
	for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
		elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
		offset += bytesRead
		// Have we now reached the end of the message
		if offset >= datalen {
			parmAvail = false
		}

		switch elem.Parameter {
		case ibmmq.MQIACF_USAGE_TYPE:
			v := int32(elem.Int64Value[0])
			switch v {
			case ibmmq.MQIACF_USAGE_BUFFER_POOL, ibmmq.MQIACF_USAGE_PAGESET:
				responseType = v
			default:
				return ""
			}

		case ibmmq.MQIACF_BUFFER_POOL_ID:
			bpId = strconv.FormatInt(elem.Int64Value[0], 10)
		case ibmmq.MQIA_PAGESET_ID:
			psId = strconv.FormatInt(elem.Int64Value[0], 10)
		case ibmmq.MQIACF_BUFFER_POOL_LOCATION:
			v := elem.Int64Value[0]
			switch int32(v) {
			case ibmmq.MQBPLOCATION_ABOVE:
				bpLocation = "Above"
			case ibmmq.MQBPLOCATION_BELOW:
				bpLocation = "Below"
			case ibmmq.MQBPLOCATION_SWITCHING_ABOVE:
				bpLocation = "Switching Above"
			case ibmmq.MQBPLOCATION_SWITCHING_BELOW:
				bpLocation = "Switching Below"
			}

		case ibmmq.MQIACF_PAGECLAS:
			v := elem.Int64Value[0]
			switch int32(v) {
			case ibmmq.MQPAGECLAS_4KB:
				bpClass = "4KB"
			case ibmmq.MQPAGECLAS_FIXED4KB:
				bpClass = "Fixed4KB"
			}
		}
	}

	// The DISPLAY USAGE command (with no qualifiers) returns two types of response.
	// Buffer pool usage and pageset usage are both reported. We can use the responseType
	// to work with both in a single pass and update separate blocks of data.
	if responseType == ibmmq.MQIACF_USAGE_BUFFER_POOL {

		// Create a unique key for this instance
		key = bpId

		UsageBpStatus.Attributes[ATTR_BP_ID].Values[key] = newStatusValueString(bpId)
		UsageBpStatus.Attributes[ATTR_BP_LOCATION].Values[key] = newStatusValueString(bpLocation)
		UsageBpStatus.Attributes[ATTR_BP_CLASS].Values[key] = newStatusValueString(bpClass)

		parmAvail = true
		// And then re-parse the message so we can store the metrics now knowing the map key
		offset = 0
		for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
			elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
			offset += bytesRead
			// Have we now reached the end of the message
			if offset >= datalen {
				parmAvail = false
			}

			statusGetIntAttributes(UsageBpStatus, elem, key)
		}
	} else {
		// Create a unique key for this instance
		key = psId

		UsagePsStatus.Attributes[ATTR_PS_ID].Values[key] = newStatusValueString(psId)
		UsagePsStatus.Attributes[ATTR_PS_BPID].Values[key] = newStatusValueString(bpId)

		parmAvail = true
		// And then re-parse the message so we can store the metrics now knowing the map key
		offset = 0
		for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
			elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
			offset += bytesRead
			// Have we now reached the end of the message
			if offset >= datalen {
				parmAvail = false
			}

			statusGetIntAttributes(UsagePsStatus, elem, key)
		}
	}
	return key
}

// Return a standardised value. If the attribute indicates that something
// special has to be done, then do that. Otherwise just make sure it's a non-negative
// value of the correct datatype
func UsageNormalise(attr *StatusAttribute, v int64) float64 {
	return statusNormalise(attr, v)
}
