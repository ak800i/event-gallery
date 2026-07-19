import type { UnknownPlugin } from '@uppy/core';
import type { Body, CompanionClientProvider, CompanionClientSearchProvider, CompanionFile, Meta, RemoteUppyFile } from '@uppy/utils';
declare const companionFileToUppyFile: <M extends Meta, B extends Body>(file: CompanionFile, plugin: UnknownPlugin<M, B>, provider: CompanionClientProvider | CompanionClientSearchProvider) => RemoteUppyFile<M, B>;
export default companionFileToUppyFile;
//# sourceMappingURL=companionFileToUppyFile.d.ts.map