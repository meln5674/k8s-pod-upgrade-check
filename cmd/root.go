/*
Copyright © 2026 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"time"

	goflag "flag"

	"github.com/blang/semver/v4"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	k8sinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"go.podman.io/image/v5/docker"
	dockerref "go.podman.io/image/v5/docker/reference"
	skopeoTypes "go.podman.io/image/v5/types"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "k8s-img-version-watcher",
	Short: "Watch Pods for new image versions available",
	// 	Long: `A longer description that spans multiple lines and likely contains
	// examples and usage of using your application. For example:
	//
	// Cobra is a CLI library for Go that empowers applications.
	// This application is a tool to generate the needed files
	// to quickly create a Cobra application.`,

	RunE: func(cmd *cobra.Command, args []string) error {

		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &k8sOverrides)
		k8scfg, err := kubeConfig.ClientConfig()
		k8sclient, err := kubernetes.NewForConfig(k8scfg)
		if err != nil {
			return err
		}

		factory := k8sinformers.NewSharedInformerFactory(k8sclient, syncInterval)
		informer := factory.Core().V1().Pods()

		checker := Checker{
			ctx:           context.TODO(),
			k8sclient:     k8sclient,
			checkInterval: checkInterval,
		}
		informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    checker.goCheckVersions,
			UpdateFunc: checker.goCheckVersions2,
		})

		factory.Start(wait.NeverStop)
		factory.WaitForCacheSync(wait.NeverStop)

		<-checker.ctx.Done()
		return nil
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

var (
	k8sOverrides   clientcmd.ConfigOverrides
	kubeconfigPath string
	allNamespaces  bool
	namespaces     []string

	klogFlags *goflag.FlagSet

	checkInterval time.Duration
	syncInterval  time.Duration
)

func init() {
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	// rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.k8s-img-version-watcher.yaml)")

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	clientcmd.BindOverrideFlags(&k8sOverrides, rootCmd.Flags(), clientcmd.RecommendedConfigOverrideFlags(""))
	klogFlags = goflag.NewFlagSet("", goflag.PanicOnError)
	klog.InitFlags(klogFlags)
	rootCmd.Flags().AddGoFlagSet(klogFlags)

	rootCmd.Flags().DurationVar(&checkInterval, "check-interval", 24*time.Hour, "time between update checks for the same pod")
	rootCmd.Flags().DurationVar(&syncInterval, "sync-interval", 15*time.Minute, "time between pods syncs from the apiserver")
}

type ImageRef struct {
	dockerref.NamedTagged
	Version *semver.Version
}

func ParseImageRef(s string) (ImageRef, error) {
	// imgRef, err := ref.New(s)
	imgRef, err := dockerref.ParseNormalizedNamed(s)
	if err != nil {
		return ImageRef{}, err
	}
	reg, _ := dockerref.SplitHostname(imgRef)
	if reg == "" {
		imgRef, err = dockerref.ParseNormalizedNamed("docker.io/" + s)
		if err != nil {
			return ImageRef{}, err
		}
	}

	img := ImageRef{
		NamedTagged: dockerref.TagNameOnly(imgRef).(dockerref.NamedTagged),
	}
	img.Version = TryParseSemver(img.Tag())
	return img, nil
}

func (i ImageRef) WithTag(t ImageTag) ImageRef {
	img, err := dockerref.WithTag(i.NamedTagged, t.Tag)
	if err != nil {
		// This should never happen?
		panic(err)
	}
	return ImageRef{NamedTagged: img, Version: t.Version}
}

func (i ImageRef) GetCreatedAt(ctx context.Context, sys *skopeoTypes.SystemContext) (*time.Time, error) {
	imgRef, err := docker.NewReference(i.NamedTagged)
	if err != nil {
		return nil, err
	}
	img, err := imgRef.NewImage(ctx, sys)
	if err != nil {
		return nil, err
	}
	defer img.Close()
	info, err := img.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	return info.Created, nil
}

func (i ImageRef) Split() (reg, repo, tag string) {
	reg, repo = dockerref.SplitHostname(i.NamedTagged)
	tag = i.NamedTagged.Tag()
	return
}

type ImageTag struct {
	Tag       string
	Version   *semver.Version
	CreatedAt *time.Time
}

type ImageRepo struct {
	Named dockerref.Named
	Tags  []ImageRef
}

// TryParseSemver tries to parse a version as semantic version-ish while appying a few heuristics
//  1. Filter out values that are probably dates, e.g. 20260101 is probably "jan 1, 2026", and not a major version
//     greater than 20 million
//  2. The "tolerant" parser from blang is used - leading "v" is stripped,
//     e.g. 1.2 becomes 1.2.0, 1 becomes 1.0.0
//  3. If even the tolerant parser fails, try splitting the string by "-" and then parsing tolerantly,
//     e.g. 1.2.3-foo1.2.3 will just use 1.2.3 even though the original isn't valid semver
//  4. If parsing succeeds, but there are no dots, and the number is really big (10k should be enough versions
//     for anyone, right?) Then assume its not actually a semver
func TryParseSemver(s string) *semver.Version {
	_, err := time.Parse("20060102", s)
	if err == nil {
		return nil
	}
	_, err = time.Parse("2006010203", s)
	if err == nil {
		return nil
	}
	v, err := semver.ParseTolerant(s)
	if err == nil {
		return &v
	}
	v, err = semver.ParseTolerant(strings.Split(s, "-")[0])
	if err != nil {
		return nil
	}
	if len(strings.Split(s, ".")) == 1 {
		return nil
	}
	return &v
}

const (
	lastCheckAnnotation = "meln5674.io/k8s-pod-upgrade-check-time"
)

type Checker struct {
	ctx           context.Context
	k8sclient     *kubernetes.Clientset
	checkInterval time.Duration
}

func (c Checker) goCheckVersions(obj any) {
	go c.checkVersions(obj)
}

func (c Checker) goCheckVersions2(old, new any) {
	go c.checkVersions(new)
}

func (c Checker) checkVersions(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.ErrorS(nil, "ignoring spruious non-pod object", "obj", obj)
		return
	}
	klog.InfoS("got pod", "namespace", pod.Namespace, "name", pod.Name)
	klog.V(4).InfoS("got pod", "namespace", pod.Namespace, "name", pod.Name, "pod", pod)

	lastCheckStr, ok := pod.Annotations[lastCheckAnnotation]
	if ok {
		lastCheck, err := time.Parse(time.RFC3339, lastCheckStr)
		if err == nil {
			next := lastCheck.Add(c.checkInterval)
			if next.After(time.Now()) {
				klog.V(3).InfoS("skipping recently checked pod", "namespace", pod.Namespace, "pod", pod.Name, "last", lastCheck)
				return
			}
			klog.V(3).InfoS("check interval expired", "namespace", pod.Namespace, "pod", pod.Name, "last", lastCheck, "next", next)
		} else {
			klog.V(3).InfoS("invalid last check time", "namespace", pod.Namespace, "pod", pod.Name, "last", lastCheckStr, "error", err)
		}
	} else {
		klog.V(3).InfoS("unchecked pod", "namespace", pod.Namespace, "pod", pod.Name)
	}

	// 0. Check for last check time annotation, ignore if not enough time has passed
	// 1. Get all images from pod, split into repo and tag
	// 2. Dedup all repos
	// 3. List tags for all repos
	// 4a. For non-semver tags, find the first and last tags, by creation time, after the current tag
	// 4b. For semver tags, find the latest versions with the same minor and subminor versions, as well as the latest versions of each major version afterwards.
	// 5. Emit events, and annotate pod with the time

	// TODO: Load mapping for "un-mirroring" repositories to their upstreams
	// TODO: Cache tag lists for repos
	// TODO: Check repos concurrently
	// TODO: Annotations to pin major/minor semver (i.e. ignore higher versions)
	// TODO: Collate events by using pod.<hash of image> as event name
	// TODO: Annotation to let pod specify the actual semver for its containers
	// TODO: Dockerfile and helm chart
	// TODO: Make patch optional, and just cache last check by pod in memory (maybe in a configmap?)
	// TODO: Make CreatedAt optional and opt-in

	nContainers := len(pod.Spec.InitContainers) + len(pod.Spec.Containers)
	containerImages := make(map[string]ImageRef, nContainers)
	repos := make(map[string]map[string]*ImageRepo, nContainers)

	containers := make([]corev1.Container, 0, nContainers)
	containers = append(containers, pod.Spec.InitContainers...)
	containers = append(containers, pod.Spec.Containers...)

	for _, container := range containers {
		img, err := ParseImageRef(container.Image)
		if err != nil {
			klog.InfoS("ignoring invalid image", "ns", pod.Namespace, "pod", pod.Name, "container", container.Name, "image", container.Image, "error", err)
			continue
		}
		containerImages[container.Name] = img
		reg, repo := dockerref.SplitHostname(img.NamedTagged)
		ensureMap(repos, reg, nContainers)
		if repos[reg][repo] == nil {
			repos[reg][repo] = &ImageRepo{
				Named: dockerref.TrimNamed(img),
			}
		}
		repos[reg][repo].Tags = append(repos[reg][repo].Tags, img)
	}

	for reg, repos := range repos {
		for repoName, repo := range repos {
			repoRef, err := docker.NewReferenceUnknownDigest(repo.Named)
			if err != nil {
				klog.ErrorS(err, "unexpected error", "registry", reg, "repository", repoName)
			}
			rawTags, err := docker.GetRepositoryTags(c.ctx, nil, repoRef)
			if err != nil {
				klog.ErrorS(err, "could not list repository tags", "registry", reg, "repository", repo)
				continue
			}
			tags := make([]ImageTag, len(rawTags))
			for ix, rawTag := range rawTags {
				tags[ix] = ImageTag{
					Tag:     rawTag,
					Version: TryParseSemver(rawTag),
				}
			}
			for _, ref := range repo.Tags {
				newer := make([]ImageTag, 0, 3)
				if ref.Version != nil {
					latestMajors := make(map[uint64]ImageTag, 1)
					latestMinor := ImageTag{Tag: ref.Tag(), Version: ref.Version}
					latestSubminor := ImageTag{Tag: ref.Tag(), Version: ref.Version}
					for _, tag := range tags {
						if tag.Version == nil {
							continue
						}
						// earlier major, ignore
						if ref.Version.Major > tag.Version.Major {
							continue
						}
						// later major, record for major if higher than latest known for that major
						if ref.Version.Major < tag.Version.Major {
							if latestMajor, ok := latestMajors[tag.Version.Major]; !ok || tag.Version.GT(*latestMajor.Version) {
								latestMajors[tag.Version.Major] = tag
							}
							continue
						}
						// same major, lower minor, ignore
						if ref.Version.Minor > tag.Version.Minor {
							continue
						}
						// same major, higher minor, record if higher than latest known for minor
						if ref.Version.Minor < tag.Version.Major {
							if tag.Version.GT(*latestMinor.Version) {
								latestMinor = tag
							}
							continue
						}
						// same major, same minor, record if higher subminor
						if tag.Version.GT(*latestSubminor.Version) {
							latestSubminor = tag
						}
					}

					if ref.Version.LT(*latestSubminor.Version) {
						newer = append(newer, latestSubminor)
					}
					if ref.Version.LT(*latestMinor.Version) {
						newer = append(newer, latestMinor)
					}
					for _, major := range slices.Sorted(maps.Keys(latestMajors)) {
						if ref.Version.LT(*latestMajors[major].Version) {
							newer = append(newer, latestMajors[major])
						}
					}
					// for ix, suggestion := range newer {
					// 	newer[ix].CreatedAt, err = ref.WithTag(suggestion).GetCreatedAt(c.ctx, nil)
					// 	if err != nil {
					// 		klog.ErrorS(err, "failed to get image createdAt", "registry", reg, "repository", repoName, "tag", ref.Tag())
					// 	}
					// }
				} else {
					// TODO: Non-semver
				}
				if len(newer) == 0 {
					klog.V(3).InfoS("no new images available", "registry", reg, "repository", repoName, "tag", ref.Tag(), "version", ref.Version)
					continue
				}
				var msg strings.Builder
				msg.WriteString("New image version(s) available: ")
				msg.WriteString(ref.String())
				msg.WriteString(" ->")
				for ix, suggestion := range newer[:min(len(newer), 5)] {
					if ix != 0 {
						msg.WriteString(",")
					}
					msg.WriteString(" ")
					msg.WriteString(suggestion.Tag)
					if suggestion.CreatedAt != nil {
						msg.WriteString(" (")
						msg.WriteString(suggestion.CreatedAt.Format(time.RFC3339))
						msg.WriteString(")")
					}
				}
				if len(newer) > 5 {
					fmt.Fprintf(&msg, " and %d more", len(newer)-5)
				}
				klog.InfoS("new images available",
					"namespace", pod.Namespace, "pod", pod.Name,
					"registry", reg, "repository", repoName, "tag", ref.Tag(),
					"version", ref.Version, "available", newer,
				)
				_, err := c.k8sclient.CoreV1().Events(pod.Namespace).Create(c.ctx,
					&corev1.Event{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "k8s-pod-upgrade-check-",
						},
						InvolvedObject: corev1.ObjectReference{
							APIVersion:      "v1",
							Kind:            "pod",
							Namespace:       pod.Namespace,
							Name:            pod.Name,
							UID:             pod.UID,
							ResourceVersion: pod.ResourceVersion,
						},
						Source: corev1.EventSource{
							Component: "k8s-pod-upgrade-check",
						},
						Type:           "Normal",
						Message:        msg.String(),
						FirstTimestamp: metav1.Now(),
						LastTimestamp:  metav1.Now(),
					}, metav1.CreateOptions{})
				if err != nil {
					klog.ErrorS(err, "failed to create event")
				}

			}
		}
	}

	_, err := c.k8sclient.CoreV1().Pods(pod.Namespace).Patch(c.ctx, pod.Name, types.StrategicMergePatchType,
		fmt.Appendf(nil, `{"metadata":{"annotations":{"%s": "%s"}}}`, lastCheckAnnotation, time.Now().Format(time.RFC3339)),
		metav1.PatchOptions{},
	)
	if err != nil {
		klog.ErrorS(err, "failed to patch pod with annotation")
	}
}

func ensureMap[K1, K2 comparable, V any](m map[K1]map[K2]V, k K1, n int) map[K2]V {
	if m2, ok := m[k]; ok {
		return m2
	}
	m2 := make(map[K2]V, n)
	m[k] = m2
	return m2
}
