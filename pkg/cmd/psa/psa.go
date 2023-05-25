package psa

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	psapi "k8s.io/pod-security-admission/api"
)

var podControllers = map[string]struct{}{
	"Deployment":  empty,
	"DaemonSet":   empty,
	"StatefulSet": empty,
	"CronJob":     empty,
	"Job":         empty,
}

// Run attempts to update the namespace psa enforce label to the psa audit value.
func (o *PSAOptions) Run() error {
	// Get a list of all the namespaces.
	namespaceList, err := o.getNamespaces()
	if err != nil {
		return fmt.Errorf("failed to get namespaces: %w", err)
	}

	podSecurityViolations := PodSecurityViolationList{}
	// Gather all the warnings for each namespace, when enforcing audit-level.
	for _, namespace := range namespaceList.Items {
		psv, err := o.checkNamespacePodSecurity(&namespace)
		if err != nil {
			return fmt.Errorf("failed to check namespace %q: %w", namespace.Name, err)
		}
		if psv == nil {
			continue
		}

		klog.V(4).Infof(
			"Pod %q has pod security violations, gathering Pod and Deployment Resources",
			psv.PodName,
			namespace.Name,
		)

		psv.Pod, err = o.client.CoreV1().
			Pods(namespace.Name).
			Get(context.Background(), psv.PodName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf(
				"failed to get pod %q from %q, which violates %q: %w",
				psv.PodName,
				namespace.Name,
				psv.Violations[0],
				err,
			)
		}

		psv.PodControllers, err = o.getPodControllers(psv.Pod)
		if err != nil {
			return fmt.Errorf("failed to get pod controller: %w", err)
		}

		podSecurityViolations.Items = append(podSecurityViolations.Items, *psv)
	}

	if len(podSecurityViolations.Items) == 0 {
		return nil
	}

	if err := o.printObj(&podSecurityViolations, o.IOStreams.Out); err != nil {
		return fmt.Errorf("failed to print pod security violations: %w", err)
	}

	if o.quiet {
		return nil
	}

	return fmt.Errorf("found %d pod security violations", len(podSecurityViolations.Items))
}

// checkNamespacePodSecurity collects the pod security violations for a given
// namespace on a stricter pod security enforcement.
func (o *PSAOptions) checkNamespacePodSecurity(ns *corev1.Namespace) (*PodSecurityViolation, error) {
	nsCopy := ns.DeepCopy()

	// Update the pod security enforcement for the dry run.
	nsCopy.Labels[psapi.EnforceLevelLabel] = o.level

	klog.V(4).Infof("Checking nsCopy %q for violations at level %q", nsCopy.Name, o.level)

	// Make a server-dry-run update on the nsCopy with the audit-level value.
	_, err := o.client.CoreV1().
		Namespaces().
		Update(
			context.Background(),
			nsCopy,
			metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}},
		)
	if err != nil {
		return nil, err
	}

	// Get the warnings from the server-dry-run update.
	warnings := o.warnings.PopAll()
	if len(warnings) == 0 {
		return nil, nil
	}

	return parseWarnings(warnings), nil
}

// getNamespaces returns the namespace that should be checked for pod security.
// It could be given by the flag. Defaults to all namespaces.
func (o *PSAOptions) getNamespaces() (*corev1.NamespaceList, error) {
	if o.allNamespaces {
		namespaceList, err := o.client.CoreV1().
			Namespaces().
			List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %w", err)
		}

		return namespaceList, nil
	}

	// Get the corev1.Namespace representation of the given namespaces.
	// Also validate that those namespaces exist.
	ns, err := o.client.CoreV1().
		Namespaces().
		Get(context.Background(), o.namespace, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace %q: %w", o.namespace, err)
	}

	return &corev1.NamespaceList{
		Items: []corev1.Namespace{*ns},
	}, nil
}

// getPodControllers gets the deployment of a pod.
func (o *PSAOptions) getPodControllers(pod *corev1.Pod) ([]any, error) {
	if len(pod.ObjectMeta.OwnerReferences) == 0 {
		return nil, nil
	}

	podControllers := []any{}
	for _, parent := range pod.ObjectMeta.OwnerReferences {
		any, err := o.getPodController(pod, parent)
		if err != nil {
			return nil, fmt.Errorf("failed to get pod controller: %w", err)
		}
		if any != nil {
			podControllers = append(podControllers, any)
		}
	}

	return podControllers, nil
}

// getPodController gets the deployment of a pod.
func (o *PSAOptions) getPodController(pod *corev1.Pod, parent metav1.OwnerReference) (any, error) {
	// If the pod is owned by a ReplicaSet, get the ReplicaSet's owner.
	if parent.Kind == "ReplicaSet" {
		replicaSet, err := o.client.AppsV1().
			ReplicaSets(pod.Namespace).
			Get(context.Background(), parent.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get ReplicaSet %q: %w", parent.Name, err)
		}

		if len(replicaSet.ObjectMeta.OwnerReferences) == 0 {
			return nil, nil
		}

		parent = replicaSet.ObjectMeta.OwnerReferences[0]
	}

	if _, ok := podControllers[parent.Kind]; !ok {
		return nil, nil
	}

	// If the pod is owned by a Deployment, get the deployment.
	switch {
	case parent.Kind == "Deployment":
		return o.client.AppsV1().
			Deployments(pod.Namespace).
			Get(context.Background(), parent.Name, metav1.GetOptions{})
	case parent.Kind == "DaemonSet":
		return o.client.AppsV1().
			DaemonSets(pod.Namespace).
			Get(context.Background(), parent.Name, metav1.GetOptions{})
	case parent.Kind == "StatefulSet":
		return o.client.AppsV1().
			StatefulSets(pod.Namespace).
			Get(context.Background(), parent.Name, metav1.GetOptions{})
	case parent.Kind == "CronJob":
	case parent.Kind == "Job":
		return o.client.BatchV1().
			Jobs(pod.Namespace).
			Get(context.Background(), parent.Name, metav1.GetOptions{})
	}

	klog.Warningf(
		"%s isn't owned by a known pod controller: pod.Name=%s, pod.Namespace=%s, pod.OwnerReferences=%v",
		parent.Kind, pod.Name, pod.OwnerReferences, pod.ObjectMeta.OwnerReferences,
	)

	return nil, nil
}
