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
This file holds most of the calls to the MQI, so we
don't need to repeat common setups eg of MQMD or MQSD structures.
*/

import (
	"fmt"
	"github.com/ibm-messaging/mq-golang/v5/ibmmq"
)

var (
	qMgr             ibmmq.MQQueueManager
	cmdQObj          ibmmq.MQObject
	replyQObj        ibmmq.MQObject
	qMgrObject       ibmmq.MQObject
	replyQBaseName   string
	statusReplyQObj  ibmmq.MQObject
	getBuffer        = make([]byte, 32768)
	platform         int32
	commandLevel     int32
	maxHandles       int32
	resolvedQMgrName string

	tzOffsetSecs float64

	qmgrConnected = false
	queuesOpened  = false
	subsOpened    = false

	usePublications = true
	useStatus       = false
	useResetQStats  = false
)

type ConnectionConfig struct {
	ClientMode   bool
	UserId       string
	Password     string
	TZOffsetSecs float64

	UsePublications bool
	UseStatus       bool
	UseResetQStats  bool
}

// Which objects are available for subscription. How
// do we define which ones to subscribe to and filter the
// specific subscriptions.

type DiscoverObject struct {
	ObjectNames          string
	UseWildcard          bool
	SubscriptionSelector string
}

// For now, only queues are subscribable through this interface
// but there are now Application resources that might be relevant
// at some time.
type DiscoverConfig struct {
	MetaPrefix      string // Root of all meta-data discovery
	MonitoredQueues DiscoverObject
}

type MQMetricError struct {
	Err      string
	MQReturn *ibmmq.MQReturn
}

func (e MQMetricError) Error() string { return e.Err + " : " + e.MQReturn.Error() }
func (e MQMetricError) Unwrap() error { return e.MQReturn }

/*
InitConnection connects to the queue manager, and then
opens both the command queue and a dynamic reply queue
to be used for all responses including the publications
*/
func InitConnection(qMgrName string, replyQ string, cc *ConnectionConfig) error {
	var err error
	var mqreturn *ibmmq.MQReturn
	var errorString = ""

	gocno := ibmmq.NewMQCNO()
	gocsp := ibmmq.NewMQCSP()

	tzOffsetSecs = cc.TZOffsetSecs

	// Explicitly force client mode if requested. Otherwise use the "default"
	// connection mechanism depending on what is installed or configured.
	if cc.ClientMode {
		gocno.Options = ibmmq.MQCNO_CLIENT_BINDING
		// Force reconnection to only be to the same qmgr. Cannot do this with externally
		// configured (eg MQ_CONNECT_TYPE or client-only installation) connections. But
		// it is a bad idea to try to reconnect to a different queue manager.
		gocno.Options |= ibmmq.MQCNO_RECONNECT_Q_MGR
	}
	gocno.Options |= ibmmq.MQCNO_HANDLE_SHARE_BLOCK

	if cc.Password != "" {
		gocsp.Password = cc.Password
	}
	if cc.UserId != "" {
		gocsp.UserId = cc.UserId
		gocno.SecurityParms = gocsp
	}

	logDebug("Connecting to queue manager %s", qMgrName)
	qMgr, err = ibmmq.Connx(qMgrName, gocno)
	if err == nil {
		qmgrConnected = true
	} else {
		errorString = "Cannot connect to queue manager " + qMgrName
		mqreturn = err.(*ibmmq.MQReturn)
	}

	// Discover important information about the qmgr - its real name
	// and the platform type. Also check if it is at least V9 (on Distributed platforms)
	// so that monitoring will work.
	if err == nil {
		var v map[int32]interface{}

		useStatus = cc.UseStatus

		mqod := ibmmq.NewMQOD()
		openOptions := ibmmq.MQOO_INQUIRE + ibmmq.MQOO_FAIL_IF_QUIESCING

		mqod.ObjectType = ibmmq.MQOT_Q_MGR
		mqod.ObjectName = ""

		qMgrObject, err = qMgr.Open(mqod, openOptions)

		if err == nil {
			selectors := []int32{ibmmq.MQCA_Q_MGR_NAME,
				ibmmq.MQIA_COMMAND_LEVEL,
				ibmmq.MQIA_PERFORMANCE_EVENT,
				ibmmq.MQIA_MAX_HANDLES,
				ibmmq.MQIA_PLATFORM}

			v, err = qMgrObject.InqMap(selectors)
			if err == nil {
				resolvedQMgrName = v[ibmmq.MQCA_Q_MGR_NAME].(string)
				platform = v[ibmmq.MQIA_PLATFORM].(int32)
				commandLevel = v[ibmmq.MQIA_COMMAND_LEVEL].(int32)
				maxHandles = v[ibmmq.MQIA_MAX_HANDLES].(int32)
				if platform == ibmmq.MQPL_ZOS {
					usePublications = false
					useResetQStats = cc.UseResetQStats
					evEnabled := v[ibmmq.MQIA_PERFORMANCE_EVENT].(int32)
					if useResetQStats && evEnabled == 0 {
						err = fmt.Errorf("Requested use of RESET QSTATS but queue manager has PERFMEV(DISABLED)")
						errorString = "Command"
					}
				} else {
					if cc.UsePublications == true {
						if commandLevel < 900 && platform != ibmmq.MQPL_APPLIANCE {
							err = fmt.Errorf("Queue manager must be at least V9.0 for full monitoring. The ibmmq.usePublications configuration parameter can be used to permit limited monitoring.")
							errorString = "Unsupported system"
						} else {
							usePublications = cc.UsePublications
						}
					} else {
						usePublications = false
					}
				}

			}

		} else {
			errorString = "Cannot open queue manager object"
			mqreturn = err.(*ibmmq.MQReturn)
		}
	}

	// MQOPEN of the COMMAND QUEUE
	if err == nil {
		mqod := ibmmq.NewMQOD()

		openOptions := ibmmq.MQOO_OUTPUT | ibmmq.MQOO_FAIL_IF_QUIESCING

		mqod.ObjectType = ibmmq.MQOT_Q
		mqod.ObjectName = "SYSTEM.ADMIN.COMMAND.QUEUE"
		if platform == ibmmq.MQPL_ZOS {
			mqod.ObjectName = "SYSTEM.COMMAND.INPUT"
		}

		cmdQObj, err = qMgr.Open(mqod, openOptions)
		if err != nil {
			errorString = "Cannot open queue " + mqod.ObjectName
			mqreturn = err.(*ibmmq.MQReturn)
		}

	}

	// MQOPEN of a reply queue also used for subscription delivery
	if err == nil {
		mqod := ibmmq.NewMQOD()
		openOptions := ibmmq.MQOO_INPUT_AS_Q_DEF | ibmmq.MQOO_FAIL_IF_QUIESCING
		openOptions |= ibmmq.MQOO_INQUIRE
		mqod.ObjectType = ibmmq.MQOT_Q
		mqod.ObjectName = replyQ
		replyQObj, err = qMgr.Open(mqod, openOptions)
		replyQBaseName = replyQ
		if err == nil {
			queuesOpened = true
		} else {
			errorString = "Cannot open queue " + replyQ
			mqreturn = err.(*ibmmq.MQReturn)
		}
	}

	// MQOPEN of a second reply queue used for status polling
	if err == nil {
		mqod := ibmmq.NewMQOD()
		openOptions := ibmmq.MQOO_INPUT_AS_Q_DEF | ibmmq.MQOO_FAIL_IF_QUIESCING
		mqod.ObjectType = ibmmq.MQOT_Q
		mqod.ObjectName = replyQ
		statusReplyQObj, err = qMgr.Open(mqod, openOptions)
		if err != nil {
			errorString = "Cannot open queue " + replyQ
			mqreturn = err.(*ibmmq.MQReturn)
		}
	}

	if err != nil {
		return MQMetricError{Err: errorString, MQReturn: mqreturn}
	}

	return err
}

/*
EndConnection tidies up by closing the queues and disconnecting.
*/
func EndConnection() {

	// MQCLOSE all subscriptions
	if subsOpened {
		for _, cl := range Metrics.Classes {
			for _, ty := range cl.Types {
				for _, hObj := range ty.subHobj {
					hObj.Close(0)
				}
			}
		}
	}

	// MQCLOSE the queues
	if queuesOpened {
		cmdQObj.Close(0)
		replyQObj.Close(0)
		statusReplyQObj.Close(0)
		qMgrObject.Close(0)
	}

	// MQDISC regardless of other errors
	if qmgrConnected {
		qMgr.Disc()
	}

}

/*
getMessage returns a message from the replyQ. The only
parameter to the function says whether this should block
for 30 seconds or return immediately if there is no message
available. When working with the command queue, blocking is
required; when getting publications, non-blocking is better.

A 32K buffer was created at the top of this file, and should always
be big enough for what we are expecting.
*/
func getMessage(wait bool) ([]byte, error) {
	return getMessageWithHObj(wait, replyQObj)
}

func getMessageWithHObj(wait bool, hObj ibmmq.MQObject) ([]byte, error) {
	var err error
	var datalen int

	getmqmd := ibmmq.NewMQMD()
	gmo := ibmmq.NewMQGMO()
	gmo.Options = ibmmq.MQGMO_NO_SYNCPOINT
	gmo.Options |= ibmmq.MQGMO_FAIL_IF_QUIESCING
	gmo.Options |= ibmmq.MQGMO_CONVERT

	gmo.MatchOptions = ibmmq.MQMO_NONE

	if wait {
		gmo.Options |= ibmmq.MQGMO_WAIT
		gmo.WaitInterval = 30 * 1000
	}

	datalen, err = hObj.Get(getmqmd, gmo, getBuffer)

	return getBuffer[0:datalen], err
}

/*
subscribe to the nominated topic. The previously-opened
replyQ is used for publications; we do not use a managed queue here,
so that everything can be read from one queue. The object handle for the
subscription is returned so we can close it when it's no longer needed.
*/
func subscribe(topic string, pubQObj *ibmmq.MQObject) (ibmmq.MQObject, error) {
	return subscribeWithOptions(topic, pubQObj, false)
}

/*
subscribe to the nominated topic, but ask the queue manager to
allocate the replyQ for us
*/
func subscribeManaged(topic string, pubQObj *ibmmq.MQObject) (ibmmq.MQObject, error) {
	return subscribeWithOptions(topic, pubQObj, true)
}

func subscribeWithOptions(topic string, pubQObj *ibmmq.MQObject, managed bool) (ibmmq.MQObject, error) {
	var err error

	mqsd := ibmmq.NewMQSD()
	mqsd.Options = ibmmq.MQSO_CREATE
	mqsd.Options |= ibmmq.MQSO_NON_DURABLE
	mqsd.Options |= ibmmq.MQSO_FAIL_IF_QUIESCING
	if managed {
		mqsd.Options |= ibmmq.MQSO_MANAGED
	}

	mqsd.ObjectString = topic

	subObj, err := qMgr.Sub(mqsd, pubQObj)
	if err != nil {
		extraInfo := ""
		mqrc := err.(*ibmmq.MQReturn).MQRC
		switch mqrc {
		case ibmmq.MQRC_HANDLE_NOT_AVAILABLE:
			extraInfo = "You may need to increase the MAXHANDS attribute on the queue manager."
		}
		return subObj, fmt.Errorf("Error subscribing to topic '%s': %v %s", topic, err, extraInfo)
	}

	return subObj, err
}

/*
Return the current platform - the MQPL_* definition value. It
can be turned into a string if necessary via ibmmq.MQItoString("PL"...)
*/
func GetPlatform() int32 {
	return platform
}

/*
Return the current command level
*/
func GetCommandLevel() int32 {
	return commandLevel
}
