package revision

import (
	"context"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

// Client is the small typed facade the control plane uses over the dynamic
// client. Keeping it here lets the API stay lightweight without generated
// client code.
type Client struct {
	dynamic dynamic.Interface
}

func NewClient(client dynamic.Interface) *Client { return &Client{dynamic: client} }

// Dynamic exposes the underlying client to the dynamic informer factory.
func (c *Client) Dynamic() dynamic.Interface { //nolint:ireturn // client-go's informer API requires its interface.
	return c.dynamic
}

func (c *Client) resource(namespace string) dynamic.ResourceInterface { //nolint:ireturn // fake and real dynamic clients share only this interface.
	return c.dynamic.Resource(Resource()).Namespace(namespace)
}

func toUnstructured(value any) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

func fromUnstructured(obj *unstructured.Unstructured, value any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, value)
}

func (c *Client) Create(ctx context.Context, namespace string, rev *Revision) (*Revision, error) {
	rev.TypeMeta = metav1.TypeMeta{APIVersion: APIVersion(), Kind: Kind}
	obj, err := toUnstructured(rev)
	if err != nil {
		return nil, err
	}
	created, err := c.resource(namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	var out Revision
	return &out, fromUnstructured(created, &out)
}

func (c *Client) Get(ctx context.Context, namespace, name string) (*Revision, error) {
	obj, err := c.resource(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var out Revision
	return &out, fromUnstructured(obj, &out)
}

func (c *Client) List(ctx context.Context, namespace string, opts metav1.ListOptions) (*List, error) {
	obj, err := c.resource(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := &List{
		TypeMeta: metav1.TypeMeta{APIVersion: APIVersion(), Kind: Kind + "List"},
		ListMeta: metav1.ListMeta{ResourceVersion: obj.GetResourceVersion(), Continue: obj.GetContinue()},
	}
	out.Items = make([]Revision, 0, len(obj.Items))
	for i := range obj.Items {
		var revision Revision
		if err := fromUnstructured(&obj.Items[i], &revision); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, revision)
	}
	return out, nil
}

func (c *Client) Delete(ctx context.Context, namespace, name string, opts metav1.DeleteOptions) error {
	return c.resource(namespace).Delete(ctx, name, opts)
}

func (c *Client) UpdateStatus(ctx context.Context, namespace string, rev *Revision) (*Revision, error) {
	obj, err := toUnstructured(rev)
	if err != nil {
		return nil, err
	}
	updated, err := c.resource(namespace).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	var out Revision
	return &out, fromUnstructured(updated, &out)
}

// Scale uses the CRD's scale subresource, keeping cold raises and autoscaling
// independent from status writes.
func (c *Client) Scale(ctx context.Context, namespace, name string, replicas int32) error {
	scale := &autoscalingv1.Scale{
		TypeMeta:   metav1.TypeMeta{APIVersion: "autoscaling/v1", Kind: "Scale"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	}
	obj, err := toUnstructured(scale)
	if err != nil {
		return err
	}
	_, err = c.resource(namespace).Update(ctx, obj, metav1.UpdateOptions{}, "scale")
	return err
}
