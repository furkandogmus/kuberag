package controller

import (
	"context"
	"hash"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ragv1alpha1 "github.com/furkandogmus/kuberag/api/v1alpha1"
)

func appendSecretHash(ctx context.Context, c client.Client, namespace, label string, ref *ragv1alpha1.SecretKeyRef, h hash.Hash) {
	if ref == nil {
		return
	}
	_, _ = io.WriteString(h, label)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, ref.Name)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, ref.Key)
	_, _ = h.Write([]byte{0})

	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &sec); err == nil {
		if val, ok := sec.Data[ref.Key]; ok {
			_, _ = h.Write([]byte{1})
			_, _ = h.Write(val)
			_, _ = h.Write([]byte{0})
			return
		}
	}
	_, _ = h.Write([]byte{0})
}
