package drivers

import (
	"fmt"
	"io"
	"math"
	"os"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/migration"
	"github.com/lxc/lxd/lxd/operations"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/rsync"
	"github.com/lxc/lxd/shared"
	log "github.com/lxc/lxd/shared/log15"
)

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied filler function.
func (d *lvm) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	revert := revert.New()
	defer revert.Fail()

	volPath := vol.MountPath()
	err := vol.EnsureMountPath()
	if err != nil {
		return err
	}
	revert.Add(func() { os.RemoveAll(volPath) })

	err = d.createLogicalVolume(d.config["lvm.vg_name"], d.thinpoolName(), vol, d.usesThinpool())
	if err != nil {
		return errors.Wrapf(err, "Error creating LVM logical volume")
	}
	revert.Add(func() { d.DeleteVolume(vol, op) })

	// For VMs, also create the filesystem volume.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.CreateVolume(fsVol, nil, op)
		if err != nil {
			return err
		}

		revert.Add(func() { d.DeleteVolume(fsVol, op) })
	}

	if filler != nil && filler.Fill != nil {
		err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
			if vol.contentType == ContentTypeFS {
				d.logger.Debug("Running filler function", log.Ctx{"path": volPath})
				err = filler.Fill(mountPath, "")
				if err != nil {
					return err
				}
			} else {
				// Get the device path.
				devPath, err := d.GetVolumeDiskPath(vol)
				if err != nil {
					return err
				}

				// Run the filler.
				d.logger.Debug("Running filler function", log.Ctx{"dev": devPath, "path": volPath})
				err = filler.Fill(mountPath, devPath)
				if err != nil {
					return err
				}
			}

			// Run EnsureMountPath again after mounting to ensure the mount directory has the correct
			// permissions set.
			err = vol.EnsureMountPath()
			if err != nil {
				return err
			}

			return nil
		}, op)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// CreateVolumeFromBackup restores a backup tarball onto the storage device.
func (d *lvm) CreateVolumeFromBackup(vol Volume, snapshots []string, srcData io.ReadSeeker, optimizedStorage bool, op *operations.Operation) (func(vol Volume) error, func(), error) {
	return genericBackupUnpack(d, vol, snapshots, srcData, op)
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *lvm) CreateVolumeFromCopy(vol, srcVol Volume, copySnapshots bool, op *operations.Operation) error {
	var err error
	var srcSnapshots []Volume

	if copySnapshots && !srcVol.IsSnapshot() {
		// Get the list of snapshots from the source.
		srcSnapshots, err = srcVol.Snapshots(op)
		if err != nil {
			return err
		}
	}

	// We can use optimised copying when the pool is backed by an LVM thinpool.
	if d.usesThinpool() {
		err = d.copyThinpoolVolume(vol, srcVol, srcSnapshots, false)
		if err != nil {
			return err
		}

		// For VMs, also copy the filesystem volume.
		if vol.IsVMBlock() {
			srcFSVol := srcVol.NewVMBlockFilesystemVolume()
			fsVol := vol.NewVMBlockFilesystemVolume()
			return d.copyThinpoolVolume(fsVol, srcFSVol, srcSnapshots, false)
		}

		return nil
	}

	// Otherwise run the generic copy.
	return genericCopyVolume(d, nil, vol, srcVol, srcSnapshots, false, op)
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *lvm) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs migration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	if vol.contentType != ContentTypeFS {
		return ErrNotSupported
	}

	if volTargetArgs.MigrationType.FSType != migration.MigrationFSType_RSYNC {
		return ErrNotSupported
	}

	return genericCreateVolumeFromMigration(d, nil, vol, conn, volTargetArgs, preFiller, op)
}

// RefreshVolume provides same-pool volume and specific snapshots syncing functionality.
func (d *lvm) RefreshVolume(vol, srcVol Volume, srcSnapshots []Volume, op *operations.Operation) error {
	// We can use optimised copying when the pool is backed by an LVM thinpool.
	if d.usesThinpool() {
		return d.copyThinpoolVolume(vol, srcVol, srcSnapshots, true)
	}

	// Otherwise run the generic copy.
	return genericCopyVolume(d, nil, vol, srcVol, srcSnapshots, true, op)
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then this function
// will return an error.
func (d *lvm) DeleteVolume(vol Volume, op *operations.Operation) error {
	snapshots, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		return fmt.Errorf("Cannot remove a volume that has snapshots")
	}

	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	lvExists, err := d.logicalVolumeExists(volDevPath)
	if err != nil {
		return err
	}

	if lvExists {
		if vol.contentType == ContentTypeFS {
			_, err = d.UnmountVolume(vol, op)
			if err != nil {
				return errors.Wrapf(err, "Error unmounting LVM logical volume")
			}
		}

		err = d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name))
		if err != nil {
			return errors.Wrapf(err, "Error removing LVM logical volume")
		}
	}

	if vol.contentType == ContentTypeFS {
		// Remove the volume from the storage device.
		mountPath := vol.MountPath()
		err = os.RemoveAll(mountPath)
		if err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, "Error removing LVM logical volume mount path %q", mountPath)
		}

		// Although the volume snapshot directory should already be removed, lets remove it here to just in
		// case the top-level directory is left.
		err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}
	}

	// For VMs, also delete the filesystem volume.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		err := d.DeleteVolume(fsVol, op)
		if err != nil {
			return err
		}
	}

	return nil
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *lvm) HasVolume(vol Volume) bool {
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	volExists, err := d.logicalVolumeExists(volDevPath)
	if err != nil {
		return false
	}

	return volExists
}

// ValidateVolume validates the supplied volume config.
func (d *lvm) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	rules := map[string]func(value string) error{
		"block.filesystem": func(value string) error {
			if value == "" {
				return nil
			}
			return shared.IsOneOf(value, lvmAllowedFilesystems)
		},
		"lvm.stripes":      shared.IsUint32,
		"lvm.stripes.size": shared.IsSize,
	}

	err := d.validateVolume(vol, rules, removeUnknownKeys)
	if err != nil {
		return err
	}

	if d.usesThinpool() && vol.config["lvm.stripes"] != "" {
		return fmt.Errorf("lvm.stripes cannot be used with thin pool volumes")
	}

	if d.usesThinpool() && vol.config["lvm.stripes.size"] != "" {
		return fmt.Errorf("lvm.stripes.size cannot be used with thin pool volumes")
	}

	return nil
}

// UpdateVolume applies config changes to the volume.
func (d *lvm) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	if vol.contentType != ContentTypeFS {
		return ErrNotSupported
	}

	if _, changed := changedConfig["size"]; changed {
		err := d.SetVolumeQuota(vol, changedConfig["size"], nil)
		if err != nil {
			return err
		}
	}

	if _, changed := changedConfig["lvm.stripes"]; changed {
		return fmt.Errorf("lvm.stripes cannot be changed")
	}

	if _, changed := changedConfig["lvm.stripes.size"]; changed {
		return fmt.Errorf("lvm.stripes.size cannot be changed")
	}

	return nil
}

// GetVolumeUsage returns the disk space used by the volume (this is not currently supported).
func (d *lvm) GetVolumeUsage(vol Volume) (int64, error) {
	// If volume has a filesystem and is mounted we can ask the filesystem for usage.
	if vol.contentType == ContentTypeFS && shared.IsMountPoint(vol.MountPath()) {
		var stat unix.Statfs_t
		err := unix.Statfs(vol.MountPath(), &stat)
		if err != nil {
			return -1, err
		}

		return int64(stat.Blocks-stat.Bfree) * int64(stat.Bsize), nil
	} else if vol.contentType == ContentTypeBlock && d.usesThinpool() {
		// For thin pool block volumes we can calculate an approximate usage using the space allocated to
		// the volume from the thin pool.
		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
		_, usedSize, err := d.thinPoolVolumeUsage(volDevPath)
		if err != nil {
			return -1, err
		}

		return int64(usedSize), nil
	}

	return -1, ErrNotSupported
}

// SetVolumeQuota sets the quota on the volume.
func (d *lvm) SetVolumeQuota(vol Volume, size string, op *operations.Operation) error {
	// Can't do anything if the size property has been removed from volume config.
	if size == "" || size == "0" {
		return nil
	}

	newSizeBytes, err := d.roundedSizeBytesString(size)
	if err != nil {
		return err
	}

	// Read actual size of current volume.
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	oldSizeBytes, err := d.logicalVolumeSize(volDevPath)
	if err != nil {
		return err
	}

	// Get the volume group's physical extent size, as we use this to figure out if the new and old sizes are
	// going to change beyond 1 extent size, otherwise there is no point in trying to resize as LVM do it.
	vgExtentSize, err := d.volumeGroupExtentSize(d.config["lvm.vg_name"])
	if err != nil {
		return err
	}

	// Round up the number of extents required for new quota size, as this is what the lvresize tool will do.
	newNumExtents := math.Ceil(float64(newSizeBytes) / float64(vgExtentSize))
	oldNumExtents := math.Ceil(float64(oldSizeBytes) / float64(vgExtentSize))
	extentDiff := int(newNumExtents - oldNumExtents)

	// If old and new extents required are the same, nothing to do, as LVM won't resize them.
	if extentDiff == 0 {
		return nil
	}

	logCtx := log.Ctx{"dev": volDevPath, "size": fmt.Sprintf("%db", newSizeBytes)}

	// Resize filesystem if needed.
	if vol.contentType == ContentTypeFS {
		if newSizeBytes < oldSizeBytes {
			// Shrink filesystem to new size first, then shrink logical volume.
			err = shrinkFileSystem(d.volumeFilesystem(vol), volDevPath, vol, newSizeBytes)
			if err != nil {
				return err
			}
			d.logger.Debug("Logical volume filesystem shrunk", logCtx)

			err = d.resizeLogicalVolume(volDevPath, newSizeBytes)
			if err != nil {
				return err
			}
		} else if newSizeBytes > oldSizeBytes {
			// Grow logical volume to new size first, then grow filesystem to fill it.
			err = d.resizeLogicalVolume(volDevPath, newSizeBytes)
			if err != nil {
				return err
			}

			err = growFileSystem(d.volumeFilesystem(vol), volDevPath, vol)
			if err != nil {
				return err
			}
			d.logger.Debug("Logical volume filesystem grown", logCtx)
		}
	} else {
		if newSizeBytes < oldSizeBytes {
			return fmt.Errorf("You cannot shrink block volumes")
		}

		err = d.resizeLogicalVolume(volDevPath, newSizeBytes)
		if err != nil {
			return err

		}
	}

	return nil
}

// GetVolumeDiskPath returns the location of a disk volume.
func (d *lvm) GetVolumeDiskPath(vol Volume) (string, error) {
	if vol.IsVMBlock() {
		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
		return volDevPath, nil
	}

	return "", ErrNotImplemented
}

// MountVolume simulates mounting a volume. As dir driver doesn't have volumes to mount it returns
// false indicating that there is no need to issue an unmount.
func (d *lvm) MountVolume(vol Volume, op *operations.Operation) (bool, error) {
	mountPath := vol.MountPath()

	// Check if already mounted.
	if vol.contentType == ContentTypeFS && !shared.IsMountPoint(mountPath) {
		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
		mountFlags, mountOptions := resolveMountOptions(d.volumeMountOptions(vol))
		err := TryMount(volDevPath, mountPath, d.volumeFilesystem(vol), mountFlags, mountOptions)
		if err != nil {
			return false, errors.Wrapf(err, "Failed to mount LVM logical volume")
		}
		d.logger.Debug("Mounted logical volume", log.Ctx{"dev": volDevPath, "path": mountPath})

		return true, nil
	}

	// For VMs, mount the filesystem volume.
	if vol.IsVMBlock() {
		fsVol := vol.NewVMBlockFilesystemVolume()
		return d.MountVolume(fsVol, op)
	}

	return false, nil
}

// UnmountVolume simulates unmounting a volume. As dir driver doesn't have volumes to unmount it
// returns false indicating the volume was already unmounted.
func (d *lvm) UnmountVolume(vol Volume, op *operations.Operation) (bool, error) {
	mountPath := vol.MountPath()

	// Check if already mounted.
	if shared.IsMountPoint(mountPath) {
		err := TryUnmount(mountPath, 0)
		if err != nil {
			return false, errors.Wrapf(err, "Failed to unmount LVM logical volume")
		}
		d.logger.Debug("Unmounted logical volume", log.Ctx{"path": mountPath})

		return true, nil
	}

	return false, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *lvm) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)

	return vol.UnmountTask(func(op *operations.Operation) error {
		snapNames, err := d.VolumeSnapshots(vol, op)
		if err != nil {
			return err
		}

		revert := revert.New()
		defer revert.Fail()

		// Rename snapshots (change volume prefix to use new parent volume name).
		for _, snapName := range snapNames {
			snapVolName := GetSnapshotVolumeName(vol.name, snapName)
			snapVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, snapVolName)
			newSnapVolName := GetSnapshotVolumeName(newVolName, snapName)
			newSnapVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, newSnapVolName)
			err = d.renameLogicalVolume(snapVolDevPath, newSnapVolDevPath)
			if err != nil {
				return err
			}
			revert.Add(func() { d.renameLogicalVolume(newSnapVolDevPath, snapVolDevPath) })
		}

		// Rename snapshots dir if present.
		if vol.contentType == ContentTypeFS {
			srcSnapshotDir := GetVolumeSnapshotDir(d.name, vol.volType, vol.name)
			dstSnapshotDir := GetVolumeSnapshotDir(d.name, vol.volType, newVolName)
			if shared.PathExists(srcSnapshotDir) {
				err = os.Rename(srcSnapshotDir, dstSnapshotDir)
				if err != nil {
					return errors.Wrapf(err, "Error renaming LVM logical volume snapshot directory from %q to %q", srcSnapshotDir, dstSnapshotDir)
				}
				revert.Add(func() { os.Rename(dstSnapshotDir, srcSnapshotDir) })
			}
		}

		// Rename actual volume.
		newVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, newVolName)
		err = d.renameLogicalVolume(volDevPath, newVolDevPath)
		if err != nil {
			return err
		}
		revert.Add(func() { d.renameLogicalVolume(newVolDevPath, volDevPath) })

		// Rename volume dir.
		if vol.contentType == ContentTypeFS {
			srcVolumePath := GetVolumeMountPath(d.name, vol.volType, vol.name)
			dstVolumePath := GetVolumeMountPath(d.name, vol.volType, newVolName)
			err = os.Rename(srcVolumePath, dstVolumePath)
			if err != nil {
				return errors.Wrapf(err, "Error renaming LVM logical volume mount path from %q to %q", srcVolumePath, dstVolumePath)
			}
			revert.Add(func() { os.Rename(dstVolumePath, srcVolumePath) })
		}

		// For VMs, also rename the filesystem volume.
		if vol.IsVMBlock() {
			fsVol := vol.NewVMBlockFilesystemVolume()
			err = d.RenameVolume(fsVol, newVolName, op)
			if err != nil {
				return err
			}
		}

		revert.Success()
		return nil
	}, op)
}

// MigrateVolume sends a volume for migration.
func (d *lvm) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *migration.VolumeSourceArgs, op *operations.Operation) error {
	if vol.contentType != ContentTypeFS {
		return ErrNotSupported
	}

	if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_RSYNC {
		return ErrNotSupported
	}

	return d.vfsMigrateVolume(vol, conn, volSrcArgs, op)
}

// BackupVolume copies a volume (and optionally its snapshots) to a specified target path.
// This driver does not support optimized backups.
func (d *lvm) BackupVolume(vol Volume, targetPath string, _, snapshots bool, op *operations.Operation) error {
	return d.vfsBackupVolume(vol, targetPath, snapshots, op)
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *lvm) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	parentVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, parentName, snapVol.config, snapVol.poolConfig)
	snapPath := snapVol.MountPath()

	// Create the parent directory.
	err := createParentSnapshotDirIfMissing(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	// Create snapshot directory.
	err = snapVol.EnsureMountPath()
	if err != nil {
		return err
	}
	revert.Add(func() { os.RemoveAll(snapPath) })

	_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], parentVol, snapVol, true, d.usesThinpool())
	if err != nil {
		return errors.Wrapf(err, "Error creating LVM logical volume snapshot")
	}

	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name)

	revert.Add(func() {
		d.removeLogicalVolume(volDevPath)
	})

	// For VMs, also snapshot the filesystem.
	if snapVol.IsVMBlock() {
		parentFSVol := parentVol.NewVMBlockFilesystemVolume()
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], parentFSVol, fsVol, true, d.usesThinpool())
		if err != nil {
			return errors.Wrapf(err, "Error creating LVM logical volume snapshot")
		}
	}

	revert.Success()
	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device. The volName and snapshotName
// must be bare names and should not be in the format "volume/snapshot".
func (d *lvm) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	// Remove the snapshot from the storage device.
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name)
	lvExists, err := d.logicalVolumeExists(volDevPath)
	if err != nil {
		return err
	}

	if lvExists {
		_, err = d.UnmountVolume(snapVol, op)
		if err != nil {
			return errors.Wrapf(err, "Error unmounting LVM logical volume")
		}

		err = d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name))
		if err != nil {
			return errors.Wrapf(err, "Error removing LVM logical volume")
		}
	}

	// For VMs, also remove the snapshot filesystem volume.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		err = d.DeleteVolumeSnapshot(fsVol, op)
		if err != nil {
			return err
		}
	}

	// Remove the snapshot mount path from the storage device.
	snapPath := snapVol.MountPath()
	err = os.RemoveAll(snapPath)
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "Error removing LVM snapshot mount path %q", snapPath)
	}

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	err = deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	return nil
}

// MountVolumeSnapshot sets up a read-only mount on top of the snapshot to avoid accidental modifications.
func (d *lvm) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	mountPath := snapVol.MountPath()

	// Check if already mounted.
	if snapVol.contentType == ContentTypeFS && !shared.IsMountPoint(mountPath) {
		revert := revert.New()
		defer revert.Fail()

		// Default to mounting the original snapshot directly. This may be changed below if a temporary
		// snapshot needs to be taken.
		mountVol := snapVol

		// Regenerate filesystem UUID if needed. This is because some filesystems do not allow mounting
		// multiple volumes that share the same UUID. As snapshotting a volume will copy its UUID we need
		// to potentially regenerate the UUID of the snapshot now that we are trying to mount it.
		// This is done at mount time rather than snapshot time for 2 reasons; firstly snapshots need to be
		// as fast as possible, and on some filesystems regenerating the UUID is a slow process, secondly
		// we do not want to modify a snapshot in case it is corrupted for some reason, so at mount time
		// we take another snapshot of the snapshot, regenerate the temporary snapshot's UUID and then
		// mount that.
		if renegerateFilesystemUUIDNeeded(d.volumeFilesystem(snapVol)) {
			// Instantiate a new volume to be the temporary writable snapshot.
			tmpVolName := fmt.Sprintf("%s%s", snapVol.name, tmpVolSuffix)
			tmpVol := NewVolume(d, d.name, snapVol.volType, snapVol.contentType, tmpVolName, snapVol.config, snapVol.poolConfig)

			// Create writable snapshot from source snapshot named with a tmpVolSuffix suffix.
			_, err := d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], snapVol, tmpVol, false, d.usesThinpool())
			if err != nil {
				return false, errors.Wrapf(err, "Error creating temporary LVM logical volume snapshot")
			}

			revert.Add(func() {
				d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], tmpVol.volType, tmpVol.contentType, tmpVol.name))
			})

			tmpVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], tmpVol.volType, tmpVol.contentType, tmpVol.name)

			d.logger.Debug("Regenerating filesystem UUID", log.Ctx{"dev": tmpVolDevPath, "fs": d.volumeFilesystem(tmpVol)})
			err = regenerateFilesystemUUID(d.volumeFilesystem(tmpVol), tmpVolDevPath)
			if err != nil {
				return false, err
			}

			// We are going to mount the temporary volume instead.
			mountVol = tmpVol
		}

		// Finally attempt to mount the volume that needs mounting.
		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], mountVol.volType, mountVol.contentType, mountVol.name)
		mountFlags, mountOptions := resolveMountOptions(d.volumeMountOptions(snapVol))
		err := TryMount(volDevPath, mountPath, d.volumeFilesystem(mountVol), mountFlags|unix.MS_RDONLY, mountOptions)
		if err != nil {
			return false, errors.Wrapf(err, "Failed to mount LVM snapshot volume")
		}
		d.logger.Debug("Mounted logical volume snapshot", log.Ctx{"dev": volDevPath, "path": mountPath})

		revert.Success()
		return true, nil
	}

	// For VMs, mount the filesystem volume.
	if snapVol.IsVMBlock() {
		fsVol := snapVol.NewVMBlockFilesystemVolume()
		return d.MountVolumeSnapshot(fsVol, op)
	}

	return false, nil
}

// UnmountVolumeSnapshot removes the read-only mount placed on top of a snapshot.
// If a temporary snapshot volume exists then it will attempt to remove it.
func (d *lvm) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	mountPath := snapVol.MountPath()

	// Check if already mounted.
	if shared.IsMountPoint(mountPath) {
		err := TryUnmount(mountPath, 0)
		if err != nil {
			return false, errors.Wrapf(err, "Failed to unmount LVM snapshot volume")
		}
		d.logger.Debug("Unmounted logical volume snapshot", log.Ctx{"path": mountPath})

		// Check if a temporary snapshot exists, and if so remove it.
		tmpVolName := fmt.Sprintf("%s%s", snapVol.name, tmpVolSuffix)
		tmpVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, tmpVolName)
		exists, err := d.logicalVolumeExists(tmpVolDevPath)
		if err != nil {
			return true, errors.Wrapf(err, "Failed to check existence of temporary LVM snapshot volume %q", tmpVolDevPath)
		}

		if exists {
			err = d.removeLogicalVolume(tmpVolDevPath)
			if err != nil {
				return true, errors.Wrapf(err, "Failed to remove temporary LVM snapshot volume %q", tmpVolDevPath)
			}
		}

		return true, nil
	}

	return false, nil
}

// VolumeSnapshots returns a list of snapshots for the volume.
func (d *lvm) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	// We use the vfsVolumeSnapshots rather than inspecting the logical volumes themselves because the origin
	// property of an LVM snapshot can be removed/changed when restoring snapshots, such that they are no
	// marked as origin of the parent volume.
	return d.vfsVolumeSnapshots(vol, op)
}

// RestoreVolume restores a volume from a snapshot.
func (d *lvm) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	// Instantiate snapshot volume from snapshot name.
	snapVol, err := vol.NewSnapshot(snapshotName)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	// If the pool uses thinpools, then the process for restoring a snapshot is as follows:
	// 1. Rename the original volume to a temporary name (so we can revert later if needed).
	// 2. Create a writable snapshot with the original name from the snapshot being restored.
	// 3. Delete the renamed original volume.
	if d.usesThinpool() {
		_, err = d.UnmountVolume(vol, op)
		if err != nil {
			return errors.Wrapf(err, "Error unmounting LVM logical volume")
		}

		originalVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
		tmpVolName := fmt.Sprintf("%s%s", vol.name, tmpVolSuffix)
		tmpVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, tmpVolName)

		// Rename original logical volume to temporary new name so we can revert if needed.
		err = d.renameLogicalVolume(originalVolDevPath, tmpVolDevPath)
		if err != nil {
			return errors.Wrapf(err, "Error temporarily renaming original LVM logical volume")
		}

		revert.Add(func() {
			// Rename the original volume back to the original name.
			d.renameLogicalVolume(tmpVolDevPath, originalVolDevPath)
		})

		// Create writable snapshot from source snapshot named as target volume.
		_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], snapVol, vol, false, true)
		if err != nil {
			return errors.Wrapf(err, "Error restoring LVM logical volume snapshot")
		}

		volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)

		revert.Add(func() {
			d.removeLogicalVolume(volDevPath)
		})

		// If the volume's filesystem needs to have its UUID regenerated to allow mount then do so now.
		if vol.contentType == ContentTypeFS && renegerateFilesystemUUIDNeeded(d.volumeFilesystem(vol)) {
			d.logger.Debug("Regenerating filesystem UUID", log.Ctx{"dev": volDevPath, "fs": d.volumeFilesystem(vol)})
			err = regenerateFilesystemUUID(d.volumeFilesystem(vol), volDevPath)
			if err != nil {
				return err
			}
		}

		// Finally remove the original logical volume. Should always be the last step to allow revert.
		err = d.removeLogicalVolume(d.lvmDevPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, tmpVolName))
		if err != nil {
			return errors.Wrapf(err, "Error removing original LVM logical volume")
		}

		revert.Success()
		return nil
	}

	// If the pool uses classic logical volumes, then the process for restoring a snapshot is as follows:
	// 1. Mount source and target.
	// 2. Rsync source to target.
	// 3. Unmount source and target.
	err = vol.MountTask(func(mountPath string, op *operations.Operation) error {
		// Copy source to destination (mounting each volume if needed).
		err = snapVol.MountTask(func(srcMountPath string, op *operations.Operation) error {
			bwlimit := d.config["rsync.bwlimit"]
			_, err := rsync.LocalCopy(srcMountPath, mountPath, bwlimit, true)
			return err
		}, op)
		if err != nil {
			return err
		}

		// Run EnsureMountPath after mounting and syncing to ensure the mounted directory has the
		// correct permissions set.
		err = vol.EnsureMountPath()
		if err != nil {
			return err
		}

		return nil
	}, op)
	if err != nil {
		return errors.Wrapf(err, "Error restoring LVM logical volume snapshot")
	}

	revert.Success()
	return nil
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *lvm) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	volDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, snapVol.name)

	parentName, _, _ := shared.InstanceGetParentAndSnapshotName(snapVol.name)
	newSnapVolName := GetSnapshotVolumeName(parentName, newSnapshotName)
	newVolDevPath := d.lvmDevPath(d.config["lvm.vg_name"], snapVol.volType, snapVol.contentType, newSnapVolName)
	err := d.renameLogicalVolume(volDevPath, newVolDevPath)
	if err != nil {
		return errors.Wrapf(err, "Error renaming LVM logical volume")
	}

	oldPath := snapVol.MountPath()
	newPath := GetVolumeMountPath(d.name, snapVol.volType, newSnapVolName)
	err = os.Rename(oldPath, newPath)
	if err != nil {
		return errors.Wrapf(err, "Error renaming snapshot mount path from %q to %q", oldPath, newPath)
	}

	return nil
}
