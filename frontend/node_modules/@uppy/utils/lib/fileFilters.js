const hasError = (file) => 'error' in file && !!file.error;
// We don't need to re-upload already successfully uploaded files
// so let's exclude them here:
// https://github.com/transloadit/uppy/issues/5930
// This happens for example when restoring a partially finished session (e.g. using golden retriever).
const isCompleted = (file) => file.progress.uploadComplete;
export function filterFilesToUpload(files) {
    return files.filter((file) => !hasError(file) && !isCompleted(file));
}
// Don't double-emit upload-started for Golden Retriever-restored files that were already started
export function filterFilesToEmitUploadStarted(files) {
    return files.filter((file) => !file.progress?.uploadStarted || !file.isRestored);
}
