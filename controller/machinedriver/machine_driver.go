package machinedriver

import (
	"fmt"
	"strings"

	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/rancher/types/config"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sync"
)

var (
	schemaLock = sync.Mutex{}
)

const (
	driverNameLabel = "io.cattle.machine_driver.name"
)

func Register(management *config.ManagementContext) {
	machineDriverLifecycle := &Lifecycle{}
	machineDriverClient := management.Management.MachineDrivers("")
	dynamicSchemaClient := management.Management.DynamicSchemas("")
	machineDriverLifecycle.machineDriverClient = machineDriverClient
	machineDriverLifecycle.schemaClient = dynamicSchemaClient

	machineDriverClient.
		Controller().
		AddHandler(v3.NewMachineDriverLifecycleAdapter("machine-driver-controller", machineDriverClient, machineDriverLifecycle))
}

type Lifecycle struct {
	machineDriverClient v3.MachineDriverInterface
	schemaClient        v3.DynamicSchemaInterface
}

func (m *Lifecycle) Create(obj *v3.MachineDriver) (*v3.MachineDriver, error) {
	// if machine driver was created, we also activate the driver by default
	driver := NewDriver(obj.Spec.Builtin, obj.Name, obj.Spec.URL, obj.Spec.Checksum)
	if err := driver.Stage(); err != nil {
		return nil, err
	}

	if err := driver.Install(); err != nil {
		logrus.Errorf("Failed to download/install driver %s: %v", driver.Name(), err)
		return nil, err
	}

	driverName := strings.TrimPrefix(driver.Name(), "docker-machine-driver-")
	flags, err := getCreateFlagsForDriver(driverName)
	if err != nil {
		return nil, err
	}
	resourceFields := map[string]v3.Field{}
	for _, flag := range flags {
		name, field, err := flagToField(flag)
		if err != nil {
			return nil, err
		}
		resourceFields[name] = field
	}
	dynamicSchema := &v3.DynamicSchema{
		Spec: v3.DynamicSchemaSpec{
			ResourceFields: resourceFields,
		},
	}
	dynamicSchema.Name = obj.Name + "config"
	dynamicSchema.OwnerReferences = []metav1.OwnerReference{
		{
			UID:        obj.UID,
			Kind:       obj.Kind,
			APIVersion: obj.APIVersion,
			Name:       obj.Name,
		},
	}
	dynamicSchema.Labels = map[string]string{}
	dynamicSchema.Labels[driverNameLabel] = obj.Name
	_, err = m.schemaClient.Create(dynamicSchema)
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}
	if err := m.createOrUpdateMachineForEmbeddedType(dynamicSchema.Name, obj.Spec.Active); err != nil {
		return nil, err
	}
	return obj, nil
}

func (m *Lifecycle) Updated(obj *v3.MachineDriver) (*v3.MachineDriver, error) {
	// YOU MUST CALL DEEPCOPY
	if err := m.createOrUpdateMachineForEmbeddedType(obj.Name + "config", obj.Spec.Active); err != nil {
		return nil, err
	}
	return nil, nil
}

func (m *Lifecycle) Remove(obj *v3.MachineDriver) (*v3.MachineDriver, error) {
	schemas, err := m.schemaClient.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", driverNameLabel, obj.Name),
	})
	if err != nil {
		return nil, err
	}
	for _, schema := range schemas.Items {
		logrus.Infof("Deleting schema %s", schema.Name)
		if err := m.schemaClient.Delete(schema.Name, &metav1.DeleteOptions{}); err != nil {
			return nil, err
		}
		logrus.Infof("Deleting schema %s done", schema.Name)
	}
	if err := m.createOrUpdateMachineForEmbeddedType(obj.Name + "config", false); err != nil {
		return nil, err
	}
	return obj, nil
}

func (m *Lifecycle) createOrUpdateMachineForEmbeddedType(embbedType string, embedded bool) error {
	schemaLock.Lock()
	defer schemaLock.Unlock()
	machineSchema, err := m.schemaClient.Get("machineconfig", metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	} else if errors.IsNotFound(err) {
		resourceField := map[string]v3.Field{}
		if embedded {
			resourceField[embbedType] = v3.Field{
				Create:   true,
				Nullable: true,
				Update:   true,
				Type:     embbedType,
			}
		}
		dynamicSchema := &v3.DynamicSchema{}
		dynamicSchema.Name = "machineconfig"
		dynamicSchema.Spec.ResourceFields = resourceField
		// todo: add embedded and embeddedType
		_, err := m.schemaClient.Create(dynamicSchema)
		if err != nil {
			return err
		}
		return nil
	}
	if embedded {
		// if embedded we add the type to schema
		logrus.Infof("uploading %s to machine schema", embbedType)
		if machineSchema.Spec.ResourceFields == nil {
			machineSchema.Spec.ResourceFields = map[string]v3.Field{}
		}
		machineSchema.Spec.ResourceFields[embbedType] = v3.Field{
			Create:   true,
			Nullable: true,
			Update:   true,
			Type:     embbedType,
		}
	} else {
		// if not we delete it from schema
		logrus.Infof("deleting %s from machine schema", embbedType)
		delete(machineSchema.Spec.ResourceFields, embbedType)
	}

	_, err = m.schemaClient.Update(machineSchema)
	if err != nil {
		return err
	}
	return nil
}
