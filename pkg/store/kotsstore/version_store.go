package kotsstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/blang/semver"
	"github.com/mholt/archiver"
	"github.com/pkg/errors"
	kotsv1beta1 "github.com/replicatedhq/kots/kotskinds/apis/kots/v1beta1"
	downstreamtypes "github.com/replicatedhq/kots/pkg/api/downstream/types"
	versiontypes "github.com/replicatedhq/kots/pkg/api/version/types"
	apptypes "github.com/replicatedhq/kots/pkg/app/types"
	"github.com/replicatedhq/kots/pkg/filestore"
	gitopstypes "github.com/replicatedhq/kots/pkg/gitops/types"
	"github.com/replicatedhq/kots/pkg/k8sutil"
	kotsadmobjects "github.com/replicatedhq/kots/pkg/kotsadm/objects"
	kotsadmconfig "github.com/replicatedhq/kots/pkg/kotsadmconfig"
	"github.com/replicatedhq/kots/pkg/kotsutil"
	"github.com/replicatedhq/kots/pkg/kustomize"
	"github.com/replicatedhq/kots/pkg/persistence"
	rendertypes "github.com/replicatedhq/kots/pkg/render/types"
	"github.com/replicatedhq/kots/pkg/secrets"
	"github.com/replicatedhq/kots/pkg/store/types"
	"github.com/replicatedhq/kots/pkg/util"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	kuberneteserrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"
)

func (s *KOTSStore) ensureApplicationMetadata(applicationMetadata string, namespace string, upstreamURI string) error {
	clientset, err := k8sutil.GetClientset()
	if err != nil {
		return errors.Wrap(err, "failed to get clientset")
	}

	existingConfigMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), "kotsadm-application-metadata", metav1.GetOptions{})
	if err != nil {
		if !kuberneteserrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to get existing metadata config map")
		}

		metadata := []byte(applicationMetadata)
		_, err := clientset.CoreV1().ConfigMaps(namespace).Create(context.TODO(), kotsadmobjects.ApplicationMetadataConfig(metadata, namespace, upstreamURI), metav1.CreateOptions{})
		if err != nil {
			return errors.Wrap(err, "failed to create metadata config map")
		}

		return nil
	}

	if existingConfigMap.Data == nil {
		existingConfigMap.Data = map[string]string{}
	}

	existingConfigMap.Data["application.yaml"] = applicationMetadata

	_, err = clientset.CoreV1().ConfigMaps(util.PodNamespace).Update(context.Background(), existingConfigMap, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to update config map")
	}

	return nil
}

func (s *KOTSStore) IsRollbackSupportedForVersion(appID string, sequence int64) (bool, error) {
	db := persistence.MustGetDBSession()
	query := `select kots_app_spec from app_version where app_id = $1 and sequence = $2`
	row := db.QueryRow(query, appID, sequence)

	var kotsAppSpecStr sql.NullString
	if err := row.Scan(&kotsAppSpecStr); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, errors.Wrap(err, "failed to scan")
	}

	if kotsAppSpecStr.String == "" {
		return false, nil
	}

	kotsAppSpec, err := kotsutil.LoadKotsAppFromContents([]byte(kotsAppSpecStr.String))
	if err != nil {
		return false, errors.Wrap(err, "failed to load kots app from contents")
	}

	return kotsAppSpec.Spec.AllowRollback, nil
}

func (s *KOTSStore) IsIdentityServiceSupportedForVersion(appID string, sequence int64) (bool, error) {
	db := persistence.MustGetDBSession()
	query := `select identity_spec from app_version where app_id = $1 and sequence = $2`
	row := db.QueryRow(query, appID, sequence)

	var identitySpecStr sql.NullString
	if err := row.Scan(&identitySpecStr); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, errors.Wrap(err, "failed to scan")
	}

	return identitySpecStr.String != "", nil
}

func (s *KOTSStore) IsSnapshotsSupportedForVersion(a *apptypes.App, sequence int64, renderer rendertypes.Renderer) (bool, error) {
	db := persistence.MustGetDBSession()
	query := `select backup_spec from app_version where app_id = $1 and sequence = $2`
	row := db.QueryRow(query, a.ID, sequence)

	var backupSpecStr sql.NullString
	if err := row.Scan(&backupSpecStr); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, errors.Wrap(err, "failed to scan")
	}

	if backupSpecStr.String == "" {
		return false, nil
	}

	archiveDir, err := ioutil.TempDir("", "kotsadm")
	if err != nil {
		return false, errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(archiveDir)

	err = s.GetAppVersionArchive(a.ID, sequence, archiveDir)
	if err != nil {
		return false, errors.Wrap(err, "failed to get app version archive")
	}

	kotsKinds, err := kotsutil.LoadKotsKindsFromPath(archiveDir)
	if err != nil {
		return false, errors.Wrap(err, "failed to load kots kinds from path")
	}

	registrySettings, err := s.GetRegistryDetailsForApp(a.ID)
	if err != nil {
		return false, errors.Wrap(err, "failed to get registry settings for app")
	}

	// as far as I can tell, this is the only place within pkg/store that uses templating
	rendered, err := renderer.RenderFile(kotsKinds, registrySettings, a.Slug, sequence, a.IsAirgap, util.PodNamespace, []byte(backupSpecStr.String))
	if err != nil {
		return false, errors.Wrap(err, "failed to render backup spec")
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode(rendered, nil, nil)
	if err != nil {
		return false, errors.Wrap(err, "failed to decode rendered backup spec yaml")
	}
	backupSpec := obj.(*velerov1.Backup)

	annotations := backupSpec.ObjectMeta.Annotations
	if annotations == nil {
		// Backup exists and there are no annotation overrides so snapshots are enabled
		return true, nil
	}

	if exclude, ok := annotations["kots.io/exclude"]; ok && exclude == "true" {
		return false, nil
	}

	if when, ok := annotations["kots.io/when"]; ok && when == "false" {
		return false, nil
	}

	return true, nil
}

func (s *KOTSStore) GetTargetKotsVersionForVersion(appID string, sequence int64) (string, error) {
	db := persistence.MustGetDBSession()
	query := `select kots_app_spec from app_version where app_id = $1 and sequence = $2`
	row := db.QueryRow(query, appID, sequence)

	var kotsAppSpecStr sql.NullString
	if err := row.Scan(&kotsAppSpecStr); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", errors.Wrap(err, "failed to scan")
	}

	if kotsAppSpecStr.String == "" {
		return "", nil
	}

	kotsAppSpec, err := kotsutil.LoadKotsAppFromContents([]byte(kotsAppSpecStr.String))
	if err != nil {
		return "", errors.Wrap(err, "failed to load kots app from contents")
	}

	return kotsAppSpec.Spec.TargetKotsVersion, nil
}

// CreateAppVersion takes an unarchived app, makes an archive and then uploads it
// to s3 with the appID and sequence specified
func (s *KOTSStore) CreateAppVersionArchive(appID string, sequence int64, archivePath string) error {
	paths := []string{
		filepath.Join(archivePath, "upstream"),
		filepath.Join(archivePath, "base"),
		filepath.Join(archivePath, "overlays"),
	}

	skippedFilesPath := filepath.Join(archivePath, "skippedFiles")
	if _, err := os.Stat(skippedFilesPath); err == nil {
		paths = append(paths, skippedFilesPath)
	}

	tmpDir, err := ioutil.TempDir("", "kotsadm")
	if err != nil {
		return errors.Wrap(err, "failed to create temp file")
	}
	defer os.RemoveAll(tmpDir)
	fileToUpload := filepath.Join(tmpDir, "archive.tar.gz")

	tarGz := archiver.TarGz{
		Tar: &archiver.Tar{
			ImplicitTopLevelFolder: false,
		},
	}
	if err := tarGz.Archive(paths, fileToUpload); err != nil {
		return errors.Wrap(err, "failed to create archive")
	}

	f, err := os.Open(fileToUpload)
	if err != nil {
		return errors.Wrap(err, "failed to open archive file")
	}
	defer f.Close()

	outputPath := fmt.Sprintf("%s/%d.tar.gz", appID, sequence)
	err = filestore.GetStore().WriteArchive(outputPath, f)
	if err != nil {
		return errors.Wrap(err, "failed to write archive")
	}

	return nil
}

// GetAppVersionArchive will fetch the archive and extract it into the given dstPath directory name
func (s *KOTSStore) GetAppVersionArchive(appID string, sequence int64, dstPath string) error {
	// too noisy
	// logger.Debug("getting app version archive",
	// 	zap.String("appID", appID),
	// 	zap.Int64("sequence", sequence))

	path := fmt.Sprintf("%s/%d.tar.gz", appID, sequence)
	bundlePath, err := filestore.GetStore().ReadArchive(path)
	if err != nil {
		return errors.Wrap(err, "failed to read archive")
	}
	defer os.RemoveAll(bundlePath)

	tarGz := archiver.TarGz{
		Tar: &archiver.Tar{
			ImplicitTopLevelFolder: false,
		},
	}
	if err := tarGz.Unarchive(bundlePath, dstPath); err != nil {
		return errors.Wrap(err, "failed to unarchive")
	}

	return nil
}

// GetAppVersionBaseSequence returns the base sequence for a given version label.
// if the "versionLabel" param is empty or is not a valid semver, the sequence of the latest version will be returned.
func (s *KOTSStore) GetAppVersionBaseSequence(appID string, versionLabel string) (int64, error) {
	appVersions, err := s.FindAppVersions(appID)
	if err != nil {
		return -1, errors.Wrapf(err, "failed to find app versions for app %s", appID)
	}

	mockVersion := &downstreamtypes.DownstreamVersion{
		// to id the mocked version and be able to retrieve it later.
		// use "MaxInt64" so that it ends up on the top of the list if it's not a semvered version.
		Sequence: math.MaxInt64,
	}

	targetSemver, err := semver.ParseTolerant(versionLabel)
	if err == nil {
		mockVersion.Semver = &targetSemver
	}

	license, err := s.GetLatestLicenseForApp(appID)
	if err != nil {
		return -1, errors.Wrap(err, "failed to get app license")
	}

	// add to the top of the list and sort
	appVersions.AllVersions = append([]*downstreamtypes.DownstreamVersion{mockVersion}, appVersions.AllVersions...)
	downstreamtypes.SortDownstreamVersions(appVersions, license.Spec.IsSemverRequired)

	var baseVersion *downstreamtypes.DownstreamVersion
	for i, v := range appVersions.AllVersions {
		if v.Sequence == math.MaxInt64 {
			// this is our mocked version, base it off of the previous version in the sorted list (if exists).
			if i < len(appVersions.AllVersions)-1 {
				baseVersion = appVersions.AllVersions[i+1]
			}
			// remove the mocked version from the list to not affect what the latest version is in case there's no previous version to use as base.
			appVersions.AllVersions = append(appVersions.AllVersions[:i], appVersions.AllVersions[i+1:]...)
			break
		}
	}

	// if a previous version was not found, base off of the latest version
	if baseVersion == nil {
		baseVersion = appVersions.AllVersions[0]
	}

	return baseVersion.ParentSequence, nil
}

// GetAppVersionBaseArchive returns the base archive directory for a given version label.
// if the "versionLabel" param is empty or is not a valid semver, the archive of the latest version will be returned.
// the base archive directory contains data such as config values.
// caller is responsible for cleaning up the created archive dir.
// returns the path to the archive and the base sequence.
func (s *KOTSStore) GetAppVersionBaseArchive(appID string, versionLabel string) (string, int64, error) {
	baseSequence, err := s.GetAppVersionBaseSequence(appID, versionLabel)
	if err != nil {
		return "", -1, errors.Wrapf(err, "failed to get base sequence for version %s", versionLabel)
	}

	archiveDir, err := ioutil.TempDir("", "kotsadm")
	if err != nil {
		return "", -1, errors.Wrap(err, "failed to create temp dir")
	}

	err = s.GetAppVersionArchive(appID, baseSequence, archiveDir)
	if err != nil {
		return "", -1, errors.Wrap(err, "failed to get app version archive")
	}

	return archiveDir, baseSequence, nil
}

func (s *KOTSStore) CreateAppVersion(appID string, baseSequence *int64, filesInDir string, source string, skipPreflights bool, gitops gitopstypes.DownstreamGitOps) (int64, error) {
	db := persistence.MustGetDBSession()

	tx, err := db.Begin()
	if err != nil {
		return 0, errors.Wrap(err, "failed to begin")
	}
	defer tx.Rollback()

	newSequence, err := s.createAppVersion(tx, appID, baseSequence, filesInDir, source, skipPreflights, gitops)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, errors.Wrap(err, "failed to commit")
	}

	return newSequence, nil
}

func (s *KOTSStore) createAppVersion(tx *sql.Tx, appID string, baseSequence *int64, filesInDir string, source string, skipPreflights bool, gitops gitopstypes.DownstreamGitOps) (int64, error) {
	kotsKinds, err := kotsutil.LoadKotsKindsFromPath(filesInDir)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to read kots kinds")
	}

	appName := kotsKinds.KotsApplication.Spec.Title
	a, err := s.GetApp(appID)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to get app")
	}
	if appName == "" {
		appName = a.Name
	}

	appIcon := kotsKinds.KotsApplication.Spec.Icon

	if err := secrets.ReplaceSecretsInPath(filesInDir); err != nil {
		return int64(0), errors.Wrap(err, "failed to replace secrets")
	}

	newSequence, err := s.createAppVersionRecord(tx, appID, appName, appIcon, kotsKinds)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to create app version")
	}

	if err := s.CreateAppVersionArchive(appID, int64(newSequence), filesInDir); err != nil {
		return int64(0), errors.Wrap(err, "failed to create app version archive")
	}

	previousArchiveDir := ""
	if baseSequence != nil {
		previousDir, err := ioutil.TempDir("", "kotsadm")
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to create temp dir")
		}
		defer os.RemoveAll(previousDir)

		// Get the previous archive, we need this to calculate the diff
		err = s.GetAppVersionArchive(appID, *baseSequence, previousDir)
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to get previous archive")
		}

		previousArchiveDir = previousDir
	}

	registrySettings, err := s.GetRegistryDetailsForApp(appID)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to get app registry info")
	}

	downstreams, err := s.ListDownstreamsForApp(appID)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to list downstreams")
	}

	kustomizeBinPath := kotsKinds.GetKustomizeBinaryPath()

	for _, d := range downstreams {
		// there's a small chance this is not optimal, but no current code path
		// will support multiple downstreams, so this is cleaner here for now

		downstreamStatus := types.VersionPending
		if baseSequence == nil && kotsKinds.IsConfigurable() { // initial version should always require configuration (if exists) even if all required items are already set and have values (except for automated installs, which can override this later)
			downstreamStatus = types.VersionPendingConfig
		} else if kotsKinds.HasPreflights() && !skipPreflights {
			downstreamStatus = types.VersionPendingPreflight
		}
		if baseSequence != nil { // only check if the version needs configuration for later versions (not the initial one) since the config is always required for the initial version (except for automated installs, which can override that later)
			// check if version needs additional configuration
			t, err := kotsadmconfig.NeedsConfiguration(a.Slug, newSequence, a.IsAirgap, kotsKinds, registrySettings)
			if err != nil {
				return int64(0), errors.Wrap(err, "failed to check if version needs configuration")
			}
			if t {
				downstreamStatus = types.VersionPendingConfig
			}
		}

		diffSummary, diffSummaryError := "", ""
		if baseSequence != nil {
			// diff this release from the last release
			diff, err := kustomize.DiffAppVersionsForDownstream(d.Name, filesInDir, previousArchiveDir, kustomizeBinPath)
			if err != nil {
				diffSummaryError = errors.Wrap(err, "failed to diff").Error()
			} else {
				b, err := json.Marshal(diff)
				if err != nil {
					diffSummaryError = errors.Wrap(err, "failed to marshal diff").Error()
				}
				diffSummary = string(b)
			}
		}

		commitURL, err := gitops.CreateGitOpsDownstreamCommit(appID, d.ClusterID, int(newSequence), filesInDir, d.Name)
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to create gitops commit")
		}

		err = s.addAppVersionToDownstream(tx, appID, d.ClusterID, newSequence,
			kotsKinds.Installation.Spec.VersionLabel, downstreamStatus, source,
			diffSummary, diffSummaryError, commitURL, commitURL != "", skipPreflights)
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to create downstream version")
		}

		// update metadata configmap
		applicationSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "Application")
		if err != nil {
			return int64(0), errors.Wrap(err, "failed to marshal application spec")
		}

		if err := s.ensureApplicationMetadata(applicationSpec, util.PodNamespace, a.UpstreamURI); err != nil {
			return int64(0), errors.Wrap(err, "failed to get metadata config map")
		}
	}

	return newSequence, nil
}

func (s *KOTSStore) createAppVersionRecord(tx *sql.Tx, appID string, appName string, appIcon string, kotsKinds *kotsutil.KotsKinds) (int64, error) {
	// we marshal these here because it's a decision of the store to cache them in the app version table
	// not all stores will do this
	supportBundleSpec, err := kotsKinds.Marshal("troubleshoot.replicated.com", "v1beta1", "Collector")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal support bundle spec")
	}
	analyzersSpec, err := kotsKinds.Marshal("troubleshoot.replicated.com", "v1beta1", "Analyzer")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal analyzer spec")
	}
	preflightSpec, err := kotsKinds.Marshal("troubleshoot.replicated.com", "v1beta1", "Preflight")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal preflight spec")
	}

	appSpec, err := kotsKinds.Marshal("app.k8s.io", "v1beta1", "Application")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal app spec")
	}
	kotsAppSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "Application")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal kots app spec")
	}
	kotsInstallationSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "Installation")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal kots installation spec")
	}

	backupSpec, err := kotsKinds.Marshal("velero.io", "v1", "Backup")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal backup spec")
	}
	identitySpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "Identity")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal identity spec")
	}

	licenseSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "License")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal license spec")
	}
	configSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "Config")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal config spec")
	}
	configValuesSpec, err := kotsKinds.Marshal("kots.io", "v1beta1", "ConfigValues")
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to marshal configvalues spec")
	}

	newSequence, err := s.getNextAppSequence(tx, appID)
	if err != nil {
		return 0, errors.Wrap(err, "failed to get next sequence number")
	}

	var releasedAt *time.Time
	if kotsKinds.Installation.Spec.ReleasedAt != nil {
		releasedAt = &kotsKinds.Installation.Spec.ReleasedAt.Time
	}
	query := `insert into app_version (app_id, sequence, created_at, version_label, release_notes, update_cursor, channel_id, channel_name, upstream_released_at, encryption_key,
		supportbundle_spec, analyzer_spec, preflight_spec, app_spec, kots_app_spec, kots_installation_spec, kots_license, config_spec, config_values, backup_spec, identity_spec)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
		ON CONFLICT(app_id, sequence) DO UPDATE SET
		created_at = EXCLUDED.created_at,
		version_label = EXCLUDED.version_label,
		release_notes = EXCLUDED.release_notes,
		update_cursor = EXCLUDED.update_cursor,
		channel_id = EXCLUDED.channel_id,
		channel_name = EXCLUDED.channel_name,
		upstream_released_at = EXCLUDED.upstream_released_at,
		encryption_key = EXCLUDED.encryption_key,
		supportbundle_spec = EXCLUDED.supportbundle_spec,
		analyzer_spec = EXCLUDED.analyzer_spec,
		preflight_spec = EXCLUDED.preflight_spec,
		app_spec = EXCLUDED.app_spec,
		kots_app_spec = EXCLUDED.kots_app_spec,
		kots_installation_spec = EXCLUDED.kots_installation_spec,
		kots_license = EXCLUDED.kots_license,
		config_spec = EXCLUDED.config_spec,
		config_values = EXCLUDED.config_values,
		backup_spec = EXCLUDED.backup_spec,
		identity_spec = EXCLUDED.identity_spec`
	_, err = tx.Exec(query, appID, newSequence, time.Now(),
		kotsKinds.Installation.Spec.VersionLabel,
		kotsKinds.Installation.Spec.ReleaseNotes,
		kotsKinds.Installation.Spec.UpdateCursor,
		kotsKinds.Installation.Spec.ChannelID,
		kotsKinds.Installation.Spec.ChannelName,
		releasedAt,
		kotsKinds.Installation.Spec.EncryptionKey,
		supportBundleSpec,
		analyzersSpec,
		preflightSpec,
		appSpec,
		kotsAppSpec,
		kotsInstallationSpec,
		licenseSpec,
		configSpec,
		configValuesSpec,
		backupSpec,
		identitySpec)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to insert app version")
	}

	query = "update app set current_sequence = $1, name = $2, icon_uri = $3 where id = $4"
	_, err = tx.Exec(query, int64(newSequence), appName, appIcon, appID)
	if err != nil {
		return int64(0), errors.Wrap(err, "failed to update app")
	}

	return int64(newSequence), nil
}

func (s *KOTSStore) addAppVersionToDownstream(tx *sql.Tx, appID string, clusterID string, sequence int64, versionLabel string, status types.DownstreamVersionStatus, source string, diffSummary string, diffSummaryError string, commitURL string, gitDeployable bool, preflightsSkipped bool) error {
	query := `insert into app_downstream_version (app_id, cluster_id, sequence, parent_sequence, created_at, version_label, status, source, diff_summary, diff_summary_error, git_commit_url, git_deployable, preflight_skipped) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	_, err := tx.Exec(
		query,
		appID,
		clusterID,
		sequence,
		sequence,
		time.Now(),
		versionLabel,
		status,
		source,
		diffSummary,
		diffSummaryError,
		commitURL,
		gitDeployable,
		preflightsSkipped)
	if err != nil {
		return errors.Wrap(err, "failed to execute query")
	}

	return nil
}

func (s *KOTSStore) GetAppVersion(appID string, sequence int64) (*versiontypes.AppVersion, error) {
	db := persistence.MustGetDBSession()
	query := `select sequence, created_at, status, applied_at, kots_installation_spec, kots_app_spec from app_version where app_id = $1 and sequence = $2`
	row := db.QueryRow(query, appID, sequence)

	var status sql.NullString
	var deployedAt persistence.NullStringTime
	var createdAt persistence.NullStringTime
	var installationSpec sql.NullString
	var kotsAppSpec sql.NullString

	v := versiontypes.AppVersion{
		AppID: appID,
	}
	if err := row.Scan(&v.Sequence, &createdAt, &status, &createdAt, &installationSpec, &kotsAppSpec); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, errors.Wrap(err, "failed to scan")
	}

	kotsKinds := kotsutil.KotsKinds{}

	// why is this a nullstring but we don't check if it's null?
	installation, err := kotsutil.LoadInstallationFromContents([]byte(installationSpec.String))
	if err != nil {
		return nil, errors.Wrap(err, "failed to read installation spec")
	}
	kotsKinds.Installation = *installation

	if kotsAppSpec.Valid {
		kotsApp, err := kotsutil.LoadKotsAppFromContents([]byte(kotsAppSpec.String))
		if err != nil {
			return nil, errors.Wrap(err, "failed to read kotsapp spec")
		}
		if kotsApp != nil {
			kotsKinds.KotsApplication = *kotsApp
		}
	}

	v.CreatedOn = createdAt.Time
	if deployedAt.Valid {
		v.DeployedAt = &deployedAt.Time
	}

	v.KOTSKinds = &kotsKinds
	v.Status = status.String

	return &v, nil
}

func (s *KOTSStore) GetLatestAppVersion(appID string) (*versiontypes.AppVersion, error) {
	downstreamVersions, err := s.FindAppVersions(appID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to find app versions")
	}
	return s.GetAppVersion(appID, downstreamVersions.AllVersions[0].ParentSequence)
}

func (s *KOTSStore) GetAppVersionsAfter(appID string, sequence int64) ([]*versiontypes.AppVersion, error) {
	db := persistence.MustGetDBSession()
	query := `select sequence, created_at, status, applied_at, kots_installation_spec from app_version where app_id = $1 and sequence > $2`
	rows, err := db.Query(query, appID, sequence)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query")
	}
	defer rows.Close()

	var status sql.NullString
	var deployedAt sql.NullTime
	var installationSpec sql.NullString

	versions := []*versiontypes.AppVersion{}

	for rows.Next() {
		v := versiontypes.AppVersion{
			AppID: appID,
		}
		if err := rows.Scan(&v.Sequence, &v.CreatedOn, &status, &deployedAt, &installationSpec); err != nil {
			return nil, errors.Wrap(err, "failed to scan")
		}

		kotsKinds := kotsutil.KotsKinds{}

		installation, err := kotsutil.LoadInstallationFromContents([]byte(installationSpec.String))
		if err != nil {
			return nil, errors.Wrap(err, "failed to read installation spec")
		}
		kotsKinds.Installation = *installation

		if deployedAt.Valid {
			v.DeployedAt = &deployedAt.Time
		}

		v.Status = status.String

		versions = append(versions, &v)
	}

	return versions, nil
}

func (s *KOTSStore) UpdateAppVersionInstallationSpec(appID string, sequence int64, installation kotsv1beta1.Installation) error {
	ser := serializer.NewYAMLSerializer(serializer.DefaultMetaFactory, scheme.Scheme, scheme.Scheme)
	var b bytes.Buffer
	if err := ser.Encode(&installation, &b); err != nil {
		return errors.Wrap(err, "failed to encode installation")
	}

	db := persistence.MustGetDBSession()
	query := `UPDATE app_version SET kots_installation_spec = $1 WHERE app_id = $2 AND sequence = $3`
	_, err := db.Exec(query, b.String(), appID, sequence)
	if err != nil {
		return errors.Wrap(err, "failed to exec")
	}
	return nil
}

func (s *KOTSStore) GetNextAppSequence(appID string) (int64, error) {
	return s.getNextAppSequence(persistence.MustGetDBSession(), appID)
}

func (s *KOTSStore) getNextAppSequence(db queryable, appID string) (int64, error) {
	var maxSequence sql.NullInt64
	row := db.QueryRow(`select max(sequence) from app_version where app_id = $1`, appID)
	if err := row.Scan(&maxSequence); err != nil {
		return 0, errors.Wrap(err, "failed to find current max sequence in row")
	}

	newSequence := int64(0)
	if maxSequence.Valid {
		newSequence = maxSequence.Int64 + 1
	}

	return newSequence, nil
}

func (s *KOTSStore) GetCurrentUpdateCursor(appID string, channelID string) (string, string, error) {
	db := persistence.MustGetDBSession()
	query := `SELECT update_cursor, version_label FROM app_version WHERE app_id = $1 AND channel_id = $2 AND update_cursor::INT IN (
		SELECT MAX(update_cursor::INT) FROM app_version WHERE app_id = $1 AND channel_id = $2
	) ORDER BY sequence DESC LIMIT 1`
	row := db.QueryRow(query, appID, channelID)

	var updateCursor sql.NullString
	var versionLabel sql.NullString

	if err := row.Scan(&updateCursor, &versionLabel); err != nil {
		if err == sql.ErrNoRows {
			return "", "", nil
		}
		return "", "", errors.Wrap(err, "failed to scan")
	}

	return updateCursor.String, versionLabel.String, nil
}
