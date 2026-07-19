import { jsx as _jsx } from "preact/jsx-runtime";
import SearchResultItem from '../Item/components/SearchResultItem.js';
const GlobalSearchView = ({ searchResults, toggleCheckbox, openFolder, i18n, }) => {
    if (searchResults.length === 0) {
        return _jsx("div", { className: "uppy-Provider-empty", children: i18n('noFilesFound') });
    }
    return (_jsx("div", { className: "uppy-ProviderBrowser-body", children: _jsx("ul", { className: "uppy-ProviderBrowser-list", children: searchResults.map((item) => (_jsx(SearchResultItem, { i18n: i18n, item: item, toggleCheckbox: toggleCheckbox, openFolder: openFolder }, item.id))) }) }));
};
export default GlobalSearchView;
