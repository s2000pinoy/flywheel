package flywheel

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// How often flywheel will update its internal state and/or check for idle
// timeouts
const SpinINTERVAL = time.Second

// Ping - HTTP requests "ping" the flywheel goroutine. This updates the idle timeout,
// and returns the current status to the http request.
type Ping struct {
	replyTo      chan Pong
	setTimeout   time.Duration
	requestStart bool
	requestStop  bool
	noop         bool
}

// Pong - result of the ping request
type Pong struct {
	Status      int       `json:"-"`
	StatusName  string    `json:"status"`
	Err         error     `json:"error,omitempty"`
	LastStarted time.Time `json:"last-started,omitempty"`
	LastStopped time.Time `json:"last-stopped,omitempty"`
	StopAt      time.Time `json:"stop-due-at"`
}

// Flywheel struct holds all the state required by the flywheel goroutine.
type Flywheel struct {
	config      *Config
	running     bool
	pings       chan Ping
	status      int
	ready       bool
	stopAt      time.Time
	lastStarted time.Time
	lastStopped time.Time
	ec2         *ec2.EC2
	autoscaling *autoscaling.AutoScaling
	hcInterval  time.Duration
	idleTimeout time.Duration
}

// New - Create new Flywheel type
func New(config *Config) *Flywheel {

	awsConfig := &aws.Config{Region: &config.Region}
	sess := session.New(awsConfig)

	return &Flywheel{
		hcInterval:  time.Duration(config.HcInterval),
		idleTimeout: time.Duration(config.IdleTimeout),
		config:      config,
		pings:       make(chan Ping),
		stopAt:      time.Now(),
		ec2:         ec2.New(sess),
		autoscaling: autoscaling.New(sess),
	}
}

// ProxyEndpoint - retrieve the reverse proxy destination
func (fw *Flywheel) ProxyEndpoint(hostname string) string {
	vhost, ok := fw.config.Vhosts[hostname]
	if ok {
		return vhost
	}
	return fw.config.Endpoint
}

// Spin - Runs the main loop for the Flywheel.
func (fw *Flywheel) Spin() {
	hchan := make(chan int, 1)

	go fw.HealthWatcher(hchan)

	ticker := time.NewTicker(SpinINTERVAL)
	for {
		select {
		case ping := <-fw.pings:
			fw.RecvPing(&ping)
		case <-ticker.C:
			fw.Poll()
		case status := <-hchan:
			if fw.status != status {
				log.Printf("Healthcheck - status is now %v", StatusString(status))
				// Status may change from STARTED to UNHEALTHY to STARTED due
				// to things like AWS RequestLimitExceeded errors.
				// If there is an active timeout, keep it instead of resetting.
				if status == STARTED && fw.stopAt.Before(time.Now()) {
					fw.stopAt = time.Now().Add(fw.idleTimeout)
					log.Printf("Timer update. Stop scheduled for %v", fw.stopAt)
				}
				fw.status = status
			}
		}
	}
}

// RecvPing - process user ping requests and update state if needed
func (fw *Flywheel) RecvPing(ping *Ping) {
	var pong Pong

	ch := ping.replyTo
	defer close(ch)

	switch fw.status {
	case STOPPED:
		if ping.requestStart {
			pong.Err = fw.Start()
		}

	case STARTED:
		if ping.noop {
			// Status requests, etc. Don't update idle timer
		} else if ping.requestStop {
			pong.Err = fw.Stop()
		} else if int64(ping.setTimeout) != 0 {
			fw.stopAt = time.Now().Add(ping.setTimeout)
			log.Printf("Timer update. Stop scheduled for %v", fw.stopAt)
		} else {
			fw.stopAt = time.Now().Add(fw.idleTimeout)
			log.Printf("Timer update. Stop scheduled for %v", fw.stopAt)
		}
	}

	pong.Status = fw.status
	pong.StatusName = StatusString(fw.status)
	pong.LastStarted = fw.lastStarted
	pong.LastStopped = fw.lastStopped
	pong.StopAt = fw.stopAt

	ch <- pong
}

// Poll - The periodic check for starting/stopping state transitions and idle
// timeouts
func (fw *Flywheel) Poll() {
	switch fw.status {
	case STARTED:
		if time.Now().After(fw.stopAt) {
			fw.Stop()
			log.Print("Idle timeout - shutting down")
			fw.status = STOPPING
		}

	case STOPPING:
		if fw.ready {
			log.Print("Shutdown complete")
			fw.status = STOPPED
		}

	case STARTING:
		if fw.ready {
			fw.status = STARTED
			fw.stopAt = time.Now().Add(fw.idleTimeout)
			log.Printf("Startup complete. Stop scheduled for %v", fw.stopAt)
		}
	}
}

// Start all the resources managed by the flywheel.
func (fw *Flywheel) Start() error {
	fw.lastStarted = time.Now()
	log.Print("Startup beginning")

	var err error
	err = fw.startInstances()

	if err == nil {
		err = fw.unterminateAutoScaling()
	}
	if err == nil {
		err = fw.startAutoScaling()
	}

	if err != nil {
		log.Printf("Error starting: %v", err)
		return err
	}

	fw.ready = false
	fw.stopAt = time.Now().Add(fw.idleTimeout)
	fw.status = STARTING
	return nil
}

// Start EC2 instances
func (fw *Flywheel) startInstances() error {
	if len(fw.config.Instances) == 0 {
		return nil
	}
	log.Printf("Starting instances %v", fw.config.Instances)
	_, err := fw.ec2.StartInstances(
		&ec2.StartInstancesInput{
			InstanceIds: fw.config.AwsInstances(),
		},
	)
	return err
}

// UnterminateAutoScaling - Restore autoscaling group instances
func (fw *Flywheel) unterminateAutoScaling() error {
	var err error
	for groupName, size := range fw.config.AutoScaling.Terminate {
		log.Printf("Restoring autoscaling group %s", groupName)
		_, err = fw.autoscaling.UpdateAutoScalingGroup(
			&autoscaling.UpdateAutoScalingGroupInput{
				AutoScalingGroupName: &groupName,
				MaxSize:              &size,
				MinSize:              &size,
			},
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// Start EC2 instances in a suspended autoscale group
// @note The autoscale group isn't unsuspended here. It's done by the
//       healthcheck once all the instances are healthy.
func (fw *Flywheel) startAutoScaling() error {
	for _, groupName := range fw.config.AutoScaling.Stop {
		log.Printf("Starting autoscaling group %s", groupName)

		resp, err := fw.autoscaling.DescribeAutoScalingGroups(
			&autoscaling.DescribeAutoScalingGroupsInput{
				AutoScalingGroupNames: []*string{&groupName},
			},
		)
		if err != nil {
			return err
		}

		group := resp.AutoScalingGroups[0]

		instanceIds := []*string{}
		for _, instance := range group.Instances {
			instanceIds = append(instanceIds, instance.InstanceId)
		}

		_, err = fw.ec2.StartInstances(
			&ec2.StartInstancesInput{
				InstanceIds: instanceIds,
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// Stop all resources managed by the flywheel
func (fw *Flywheel) Stop() error {
	fw.lastStopped = time.Now()

	var err error
	err = fw.stopInstances()

	if err == nil {
		err = fw.terminateAutoScaling()
	}
	if err == nil {
		err = fw.stopAutoScaling()
	}

	if err != nil {
		log.Printf("Error stopping: %v", err)
		return err
	}

	fw.ready = false
	fw.status = STOPPING
	fw.stopAt = fw.lastStopped
	return nil
}

// Stop EC2 instances
func (fw *Flywheel) stopInstances() error {
	if len(fw.config.Instances) == 0 {
		return nil
	}
	log.Printf("Stopping instances %v", fw.config.Instances)
	_, err := fw.ec2.StopInstances(
		&ec2.StopInstancesInput{
			InstanceIds: fw.config.AwsInstances(),
		},
	)
	return err
}

// Suspend ReplaceUnhealthy in an autoscale group and stop the instances.
func (fw *Flywheel) stopAutoScaling() error {
	for _, groupName := range fw.config.AutoScaling.Stop {
		log.Printf("Stopping autoscaling group %s", groupName)

		resp, err := fw.autoscaling.DescribeAutoScalingGroups(
			&autoscaling.DescribeAutoScalingGroupsInput{
				AutoScalingGroupNames: []*string{&groupName},
			},
		)
		if err != nil {
			return err
		}

		group := resp.AutoScalingGroups[0]

		_, err = fw.autoscaling.SuspendProcesses(
			&autoscaling.ScalingProcessQuery{
				AutoScalingGroupName: group.AutoScalingGroupName,
				ScalingProcesses: []*string{
					aws.String("ReplaceUnhealthy"),
				},
			},
		)
		if err != nil {
			return err
		}

		instanceIds := []*string{}
		for _, instance := range group.Instances {
			instanceIds = append(instanceIds, instance.InstanceId)
		}

		_, err = fw.ec2.StopInstances(
			&ec2.StopInstancesInput{
				InstanceIds: instanceIds,
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// Reduce autoscaling min/max instances to 0, causing the instances to be terminated.
func (fw *Flywheel) terminateAutoScaling() error {
	var err error
	var zero int64
	for groupName := range fw.config.AutoScaling.Terminate {
		log.Printf("Terminating autoscaling group %s", groupName)
		_, err = fw.autoscaling.UpdateAutoScalingGroup(
			&autoscaling.UpdateAutoScalingGroupInput{
				AutoScalingGroupName: &groupName,
				MaxSize:              &zero,
				MinSize:              &zero,
			},
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// WriteStatusFile - Before we exit the application we write the current state
func (fw *Flywheel) WriteStatusFile(statusFile string) {
	var pong Pong

	fd, err := os.OpenFile(statusFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Unable to write status file: %s", err)
		return
	}
	defer fd.Close()

	pong.Status = fw.status
	pong.StatusName = StatusString(fw.status)
	pong.LastStarted = fw.lastStarted
	pong.LastStopped = fw.lastStopped

	buf, err := json.Marshal(pong)
	if err != nil {
		log.Printf("Unable to write status file: %s", err)
		return
	}

	_, err = fd.Write(buf)
	if err != nil {
		log.Printf("Unable to write status file: %s", err)
		return
	}
}

// ReadStatusFile load status from the status file
func (fw *Flywheel) ReadStatusFile(statusFile string) {
	fd, err := os.Open(statusFile)
	if err != nil {
		if err != os.ErrNotExist {
			log.Printf("Unable to load status file: %v", err)
		}
		return
	}

	stat, err := fd.Stat()
	if err != nil {
		log.Printf("Unable to load status file: %v", err)
		return
	}

	buf := make([]byte, int(stat.Size()))
	_, err = io.ReadFull(fd, buf)
	if err != nil {
		log.Printf("Unable to load status file: %v", err)
		return
	}

	var status Pong
	err = json.Unmarshal(buf, &status)
	if err != nil {
		log.Printf("Unable to load status file: %v", err)
		return
	}

	fw.status = status.Status
	fw.lastStarted = status.LastStarted
	fw.lastStopped = status.LastStopped
}
