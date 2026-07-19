import { getSafeFileId } from '@uppy/utils';
import companionFileToUppyFile from './companionFileToUppyFile.js';
const addFiles = (companionFiles, plugin, provider) => {
    const uppyFiles = companionFiles.map((f) => companionFileToUppyFile(f, plugin, provider));
    const filesToAdd = [];
    const filesAlreadyAdded = [];
    uppyFiles.forEach((file) => {
        if (plugin.uppy.checkIfFileAlreadyExists(getSafeFileId(file, plugin.uppy.getID()))) {
            filesAlreadyAdded.push(file);
        }
        else {
            filesToAdd.push(file);
        }
    });
    if (filesToAdd.length > 0) {
        plugin.uppy.info(plugin.uppy.i18n('addedNumFiles', { numFiles: filesToAdd.length }));
    }
    if (filesAlreadyAdded.length > 0) {
        plugin.uppy.info(`Not adding ${filesAlreadyAdded.length} files because they already exist`);
    }
    plugin.uppy.addFiles(filesToAdd);
};
export default addFiles;
