package pub_client

import (
	"context"
	"fmt"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/mq_pb"
	"github.com/seaweedfs/seaweedfs/weed/util/buffered_queue"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"log"
	"sort"
	"sync"
	"time"
)

type EachPartitionError struct {
	*mq_pb.BrokerPartitionAssignment
	Err error
	generation int
}

type EachPartitionPublishJob struct {
	*mq_pb.BrokerPartitionAssignment
	stopChan chan bool
	wg 	 sync.WaitGroup
	generation int
	inputQueue *buffered_queue.BufferedQueue[*mq_pb.DataMessage]
}
func (p *TopicPublisher) StartSchedulerThread(bootstrapBrokers []string, wg *sync.WaitGroup) error {

	if err := p.doEnsureConfigureTopic(bootstrapBrokers); err != nil {
		return fmt.Errorf("configure topic %s/%s: %v", p.namespace, p.topic, err)
	}

	log.Printf("start scheduler thread for topic %s/%s", p.namespace, p.topic)

	generation := 0
	var errChan chan EachPartitionError
	for {
		glog.V(0).Infof("lookup partitions gen %d topic %s/%s", generation+1, p.namespace, p.topic)
		if assignments, err := p.doLookupTopicPartitions(bootstrapBrokers); err == nil {
			generation++
			glog.V(0).Infof("start generation %d with %d assignments", generation, len(assignments))
			if errChan == nil {
				errChan = make(chan EachPartitionError, len(assignments))
			}
			p.onEachAssignments(generation, assignments, errChan)
		} else {
			glog.Errorf("lookup topic %s/%s: %v", p.namespace, p.topic, err)
			time.Sleep(5 * time.Second)
			continue
		}

		if generation == 1 {
			wg.Done()
		}

		// wait for any error to happen. If so, consume all remaining errors, and retry
		for {
			select {
			case eachErr := <-errChan:
				glog.Errorf("gen %d publish to topic %s/%s partition %v: %v", eachErr.generation, p.namespace, p.topic, eachErr.Partition, eachErr.Err)
				if eachErr.generation < generation {
					continue
				}
				break
			}
		}
	}
}

func (p *TopicPublisher) onEachAssignments(generation int, assignments []*mq_pb.BrokerPartitionAssignment, errChan chan EachPartitionError) {
	// TODO assuming this is not re-configured so the partitions are fixed.
	sort.Slice(assignments, func(i, j int) bool {
		return assignments[i].Partition.RangeStart < assignments[j].Partition.RangeStart
	})
	var jobs []*EachPartitionPublishJob
	hasExistingJob := len(p.jobs) == len(assignments)
	for i, assignment := range assignments {
		if assignment.LeaderBroker == "" {
			continue
		}
		if hasExistingJob {
			var existingJob *EachPartitionPublishJob
			existingJob = p.jobs[i]
			if existingJob.BrokerPartitionAssignment.LeaderBroker == assignment.LeaderBroker {
				existingJob.generation = generation
				jobs = append(jobs, existingJob)
				continue
			} else {
				if existingJob.LeaderBroker != "" {
					close(existingJob.stopChan)
					existingJob.LeaderBroker = ""
					existingJob.wg.Wait()
				}
			}
		}

		// start a go routine to publish to this partition
		job := &EachPartitionPublishJob{
			BrokerPartitionAssignment: assignment,
			stopChan: make(chan bool, 1),
			generation: generation,
			inputQueue: buffered_queue.NewBufferedQueue[*mq_pb.DataMessage](1024, true),
		}
		job.wg.Add(1)
		go func(job *EachPartitionPublishJob) {
			defer job.wg.Done()
			if err := p.doPublishToPartition(job); err != nil {
				errChan <- EachPartitionError{assignment, err, generation}
			}
		}(job)
		jobs = append(jobs, job)
		// TODO assuming this is not re-configured so the partitions are fixed.
		// better just re-use the existing job
		p.partition2Buffer.Insert(assignment.Partition.RangeStart, assignment.Partition.RangeStop, job.inputQueue)
	}
	p.jobs = jobs
}

func (p *TopicPublisher) doPublishToPartition(job *EachPartitionPublishJob) error {

	log.Printf("connecting to %v for topic partition %+v", job.LeaderBroker, job.Partition)

	grpcConnection, err := pb.GrpcDial(context.Background(), job.LeaderBroker, true, p.grpcDialOption)
	if err != nil {
		return fmt.Errorf("dial broker %s: %v", job.LeaderBroker, err)
	}
	brokerClient := mq_pb.NewSeaweedMessagingClient(grpcConnection)
	stream, err := brokerClient.PublishMessage(context.Background())
	if err != nil {
		return fmt.Errorf("create publish client: %v", err)
	}
	publishClient := &PublishClient{
		SeaweedMessaging_PublishMessageClient: stream,
		Broker:                         job.LeaderBroker,
	}
	if err = publishClient.Send(&mq_pb.PublishMessageRequest{
		Message: &mq_pb.PublishMessageRequest_Init{
			Init: &mq_pb.PublishMessageRequest_InitMessage{
				Topic: &mq_pb.Topic{
					Namespace: p.namespace,
					Name:      p.topic,
				},
				Partition: job.Partition,
				AckInterval: 128,
			},
		},
	}); err != nil {
		return fmt.Errorf("send init message: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv init response: %v", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("init response error: %v", resp.Error)
	}

	go func() {
		for {
			_, err := publishClient.Recv()
			if err != nil {
				e, ok := status.FromError(err)
				if ok && e.Code() == codes.Unknown && e.Message() == "EOF" {
					return
				}
				publishClient.Err = err
				fmt.Printf("publish to %s error: %v\n", publishClient.Broker, err)
				return
			}
		}
	}()

	for data, hasData := job.inputQueue.Dequeue(); hasData; data, hasData = job.inputQueue.Dequeue() {
		if err := publishClient.Send(&mq_pb.PublishMessageRequest{
			Message: &mq_pb.PublishMessageRequest_Data{
				Data: data,
			},
		}); err != nil {
			return fmt.Errorf("send publish data: %v", err)
		}
	}
	return nil
}

func (p *TopicPublisher) doEnsureConfigureTopic(bootstrapBrokers []string) (err error) {
	if len(bootstrapBrokers) == 0 {
		return fmt.Errorf("no bootstrap brokers")
	}
	var lastErr error
	for _, brokerAddress := range bootstrapBrokers {
		err = pb.WithBrokerGrpcClient(false,
			brokerAddress,
			p.grpcDialOption,
			func(client mq_pb.SeaweedMessagingClient) error {
				_, err := client.ConfigureTopic(context.Background(), &mq_pb.ConfigureTopicRequest{
					Topic: &mq_pb.Topic{
						Namespace: p.namespace,
						Name:      p.topic,
					},
					PartitionCount: p.config.CreateTopicPartitionCount,
				})
				return err
			})
		if err == nil {
			return nil
		} else {
			lastErr = err
		}
	}

	if lastErr != nil {
		return fmt.Errorf("configure topic %s/%s: %v", p.namespace, p.topic, err)
	}
	return nil
}

func (p *TopicPublisher) doLookupTopicPartitions(bootstrapBrokers []string) (assignments []*mq_pb.BrokerPartitionAssignment, err error) {
	if len(bootstrapBrokers) == 0 {
		return nil, fmt.Errorf("no bootstrap brokers")
	}
	var lastErr error
	for _, brokerAddress := range bootstrapBrokers {
		err := pb.WithBrokerGrpcClient(false,
			brokerAddress,
			p.grpcDialOption,
			func(client mq_pb.SeaweedMessagingClient) error {
				lookupResp, err := client.LookupTopicBrokers(context.Background(),
					&mq_pb.LookupTopicBrokersRequest{
						Topic: &mq_pb.Topic{
							Namespace: p.namespace,
							Name:      p.topic,
						},
					})
				glog.V(0).Infof("lookup topic %s/%s: %v", p.namespace, p.topic, lookupResp)

				if err != nil {
					return err
				}

				if len(lookupResp.BrokerPartitionAssignments) == 0 {
					return fmt.Errorf("no broker partition assignments")
				}

				assignments = lookupResp.BrokerPartitionAssignments

				return nil
			})
		if err == nil {
			return assignments, nil
		} else {
			lastErr = err
		}
	}

	return nil, fmt.Errorf("lookup topic %s/%s: %v", p.namespace, p.topic, lastErr)

}
