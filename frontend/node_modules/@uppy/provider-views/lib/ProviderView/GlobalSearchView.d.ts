import type { PartialTreeFile, PartialTreeFolderNode } from '@uppy/core';
import type { I18n } from '@uppy/utils';
import type ProviderView from './ProviderView.js';
interface GlobalSearchViewProps {
    searchResults: (PartialTreeFile | PartialTreeFolderNode)[];
    openFolder: ProviderView<any, any>['openSearchResultFolder'];
    toggleCheckbox: ProviderView<any, any>['toggleCheckbox'];
    i18n: I18n;
}
declare const GlobalSearchView: ({ searchResults, toggleCheckbox, openFolder, i18n, }: GlobalSearchViewProps) => import("preact").JSX.Element;
export default GlobalSearchView;
//# sourceMappingURL=GlobalSearchView.d.ts.map