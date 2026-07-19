import type { PartialTreeFile, PartialTreeFolderNode } from '@uppy/core';
import type { I18n } from '@uppy/utils';
import type ProviderView from '../../ProviderView/ProviderView.js';
interface SearchResultItemProps {
    item: PartialTreeFile | PartialTreeFolderNode;
    i18n: I18n;
    openFolder: ProviderView<any, any>['openSearchResultFolder'];
    toggleCheckbox: ProviderView<any, any>['toggleCheckbox'];
}
declare const SearchResultItem: ({ i18n, item, toggleCheckbox, openFolder, }: SearchResultItemProps) => import("preact").JSX.Element;
export default SearchResultItem;
//# sourceMappingURL=SearchResultItem.d.ts.map