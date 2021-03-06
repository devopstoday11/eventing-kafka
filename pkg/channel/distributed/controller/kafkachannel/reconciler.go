/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kafkachannel

import (
	"context"
	"fmt"
	"sync"

	"github.com/Shopify/sarama"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	kafkav1beta1 "knative.dev/eventing-kafka/pkg/apis/messaging/v1beta1"
	"knative.dev/eventing-kafka/pkg/channel/distributed/common/config"
	kafkaadmin "knative.dev/eventing-kafka/pkg/channel/distributed/common/kafka/admin"
	kafkasarama "knative.dev/eventing-kafka/pkg/channel/distributed/common/kafka/sarama"
	"knative.dev/eventing-kafka/pkg/channel/distributed/controller/constants"
	"knative.dev/eventing-kafka/pkg/channel/distributed/controller/env"
	"knative.dev/eventing-kafka/pkg/channel/distributed/controller/event"
	"knative.dev/eventing-kafka/pkg/channel/distributed/controller/util"
	kafkaclientset "knative.dev/eventing-kafka/pkg/client/clientset/versioned"
	"knative.dev/eventing-kafka/pkg/client/injection/reconciler/messaging/v1beta1/kafkachannel"
	kafkalisters "knative.dev/eventing-kafka/pkg/client/listers/messaging/v1beta1"
	kubeclient "knative.dev/pkg/client/injection/kube/client"
	"knative.dev/pkg/reconciler"
)

// Reconciler Implements controller.Reconciler for KafkaChannel Resources
type Reconciler struct {
	logger               *zap.Logger
	kubeClientset        kubernetes.Interface
	kafkaClientSet       kafkaclientset.Interface
	adminClientType      kafkaadmin.AdminClientType
	adminClient          kafkaadmin.AdminClientInterface
	environment          *env.Environment
	config               *config.EventingKafkaConfig
	saramaConfig         *sarama.Config
	kafkachannelLister   kafkalisters.KafkaChannelLister
	kafkachannelInformer cache.SharedIndexInformer
	deploymentLister     appsv1listers.DeploymentLister
	serviceLister        corev1listers.ServiceLister
	configObserver       func(configMap *corev1.ConfigMap)
	adminMutex           *sync.Mutex
}

var (
	_ kafkachannel.Interface = (*Reconciler)(nil) // Verify Reconciler Implements Interface
	_ kafkachannel.Finalizer = (*Reconciler)(nil) // Verify Reconciler Implements Finalizer
)

//
// Clear / Re-Set The Kafka AdminClient On The Reconciler
//
// Ideally we would re-use the Kafka AdminClient but due to Issues with the Sarama ClusterAdmin we're
// forced to recreate a new connection every time.  We were seeing "broken-pipe" failures (non-recoverable)
// with the ClusterAdmin after periods of inactivity.
//   https://github.com/Shopify/sarama/issues/1162
//   https://github.com/Shopify/sarama/issues/866
//
// EventHub AdminClients could be reused, and this is somewhat inefficient for them, but they are very simple
// lightweight REST clients so recreating them isn't a big deal and it simplifies the code significantly to
// not have to support both use cases.
//
func (r *Reconciler) SetKafkaAdminClient(ctx context.Context) {
	r.ClearKafkaAdminClient()
	var err error
	r.adminClient, err = kafkaadmin.CreateAdminClient(ctx, r.saramaConfig, constants.ControllerComponentName, r.adminClientType)
	if err != nil {
		r.logger.Error("Failed To Create Kafka AdminClient", zap.Error(err))
	}
}

// Clear (Close) The Reconciler's Kafka AdminClient
func (r *Reconciler) ClearKafkaAdminClient() {
	if r.adminClient != nil {
		err := r.adminClient.Close()
		if err != nil {
			r.logger.Error("Failed To Close Kafka AdminClient", zap.Error(err))
		}
		r.adminClient = nil
	}
}

// ReconcileKind Implements The Reconciler Interface & Is Responsible For Performing The Reconciliation (Creation)
func (r *Reconciler) ReconcileKind(ctx context.Context, channel *kafkav1beta1.KafkaChannel) reconciler.Event {

	r.logger.Debug("<==========  START KAFKA-CHANNEL RECONCILIATION  ==========>")

	// Add The K8S ClientSet To The Reconcile Context
	ctx = context.WithValue(ctx, kubeclient.Key{}, r.kubeClientset)

	// Don't let another goroutine clear out the admin client while we're using it in this one
	r.adminMutex.Lock()
	defer r.adminMutex.Unlock()

	// Create A New Kafka AdminClient For Each Reconciliation Attempt
	r.SetKafkaAdminClient(ctx)
	defer r.ClearKafkaAdminClient()

	// Reset The Channel's Status Conditions To Unknown (Addressable, Topic, Service, Deployment, etc...)
	channel.Status.InitializeConditions()

	// Perform The KafkaChannel Reconciliation & Handle Error Response
	r.logger.Info("Channel Owned By Controller - Reconciling", zap.Any("Channel.Spec", channel.Spec))
	err := r.reconcile(ctx, channel)
	if err != nil {
		r.logger.Error("Failed To Reconcile KafkaChannel", zap.Any("Channel", channel), zap.Error(err))
		return err
	}

	// Return Success
	r.logger.Info("Successfully Reconciled KafkaChannel", zap.Any("Channel", channel))
	channel.Status.ObservedGeneration = channel.Generation
	return reconciler.NewEvent(corev1.EventTypeNormal, event.KafkaChannelReconciled.String(), "KafkaChannel Reconciled Successfully: \"%s/%s\"", channel.Namespace, channel.Name)
}

// ReconcileKind Implements The Finalizer Interface & Is Responsible For Performing The Finalization (Topic Deletion)
func (r *Reconciler) FinalizeKind(ctx context.Context, channel *kafkav1beta1.KafkaChannel) reconciler.Event {

	r.logger.Debug("<==========  START KAFKA-CHANNEL FINALIZATION  ==========>")

	// Setup Logger
	logger := util.ChannelLogger(r.logger, channel)

	// Add The K8S ClientSet To The Reconcile Context
	ctx = context.WithValue(ctx, kubeclient.Key{}, r.kubeClientset)

	// Don't let another goroutine clear out the admin client while we're using it in this one
	r.adminMutex.Lock()
	defer r.adminMutex.Unlock()

	// Create A New Kafka AdminClient For Each Reconciliation Attempt
	r.SetKafkaAdminClient(ctx)
	defer r.ClearKafkaAdminClient()

	// Finalize The Dispatcher (Manual Finalization Due To Cross-Namespace Ownership)
	err := r.finalizeDispatcher(ctx, channel)
	if err != nil {
		logger.Info("Failed To Finalize KafkaChannel", zap.Error(err))
		return fmt.Errorf(constants.FinalizationFailedError)
	}

	// Finalize The Kafka Topic
	err = r.finalizeKafkaTopic(ctx, channel)
	if err != nil {
		logger.Error("Failed To Finalize KafkaChannel", zap.Error(err))
		return fmt.Errorf(constants.FinalizationFailedError)
	}

	// Return Success
	logger.Info("Successfully Finalized KafkaChannel")
	return reconciler.NewEvent(corev1.EventTypeNormal, event.KafkaChannelFinalized.String(), "KafkaChannel Finalized Successfully: \"%s/%s\"", channel.Namespace, channel.Name)
}

// Perform The Actual Channel Reconciliation
func (r *Reconciler) reconcile(ctx context.Context, channel *kafkav1beta1.KafkaChannel) error {

	// NOTE - The sequential order of reconciliation must be "Topic" then "Channel / Dispatcher" in order for the
	//        EventHub Cache to know the dynamically determined EventHub Namespace / Kafka Secret selected for the topic.

	// Reconcile The KafkaChannel's Kafka Topic
	err := r.reconcileKafkaTopic(ctx, channel)
	if err != nil {
		return fmt.Errorf(constants.ReconciliationFailedError)
	}

	//
	// This implementation is based on the "consolidated" KafkaChannel, and thus we're using
	// their Status tracking even though it does not align with our architecture.  We get our
	// Kafka configuration from the "Kafka Secrets" and not a ConfigMap.  Therefore, we will
	// instead check the Kafka Secret associated with the KafkaChannel here.
	//

	if len(r.adminClient.GetKafkaSecretName(util.TopicName(channel))) > 0 {
		channel.Status.MarkConfigTrue()
	} else {
		channel.Status.MarkConfigFailed(event.KafkaSecretReconciled.String(), "No Kafka Secret For KafkaChannel")
		return fmt.Errorf(constants.ReconciliationFailedError)
	}

	// Reconcile The KafkaChannel's Channel & Dispatcher Deployment/Service
	channelError := r.reconcileChannel(ctx, channel)
	dispatcherError := r.reconcileDispatcher(ctx, channel)
	if channelError != nil || dispatcherError != nil {
		return fmt.Errorf(constants.ReconciliationFailedError)
	}

	// Reconcile The KafkaChannel Itself (MetaData, etc...)
	err = r.reconcileKafkaChannel(ctx, channel)
	if err != nil {
		return fmt.Errorf(constants.ReconciliationFailedError)
	}

	// Return Success
	return nil
}

// configMapObserver is the callback function that handles changes to our ConfigMap
func (r *Reconciler) configMapObserver(configMap *corev1.ConfigMap) {
	if configMap == nil {
		r.logger.Warn("Nil ConfigMap passed to configMapObserver; ignoring")
		return
	}
	if r == nil {
		// This typically happens during startup
		r.logger.Debug("Reconciler is nil during call to configMapObserver; ignoring changes")
		return
	}

	// Enable Sarama Logging If Specified In ConfigMap
	if ekConfig, err := kafkasarama.LoadEventingKafkaSettings(configMap); err == nil && ekConfig != nil {
		kafkasarama.EnableSaramaLogging(ekConfig.Kafka.EnableSaramaLogging)
		r.logger.Debug("Updated Sarama logging", zap.Bool("Kafka.EnableSaramaLogging", ekConfig.Kafka.EnableSaramaLogging))
	} else {
		r.logger.Error("Could Not Extract Eventing-Kafka Setting From Updated ConfigMap", zap.Error(err))
	}

	// Though the new configmap could technically have changes to the eventing-kafka section
	// (aside from the Sarama logging) as well as the sarama section, we currently do not do
	// anything proactive based on configuration changes to those items.  The only component
	// in the controller that uses any of the fields after startup currently is the AdminClient,
	// which simply uses the r.saramaConfig set here whenever necessary.  This means that calling
	// env.GetEnvironment is not necessary now.  If	those settings are needed in the future, the
	// environment will also need to be re-parsed here.

	// Load the Sarama settings from our configmap, ignoring the eventing-kafka result.
	saramaConfig, err := kafkasarama.MergeSaramaSettings(nil, configMap)
	if err != nil {
		r.logger.Fatal("Failed To Load Eventing-Kafka Settings", zap.Error(err))
	}

	// Note - We're not calling UpdateSaramaConfig() here because we load the Kafka Secret
	//        from inside the AdminClient, which is currently done for every reconciliation.

	r.logger.Info("ConfigMap Changed; Updating Sarama Configuration")
	r.saramaConfig = saramaConfig
}
