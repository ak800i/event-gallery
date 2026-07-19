import { jsx as _jsx, jsxs as _jsxs } from "preact/jsx-runtime";
import classNames from 'classnames';
import ItemIcon from './ItemIcon.js';
const SearchResultItem = ({ i18n, item, toggleCheckbox, openFolder, }) => {
    const isDisabled = 'restrictionError' in item &&
        item.restrictionError != null &&
        item.status !== 'checked';
    return (_jsxs("li", { className: classNames('uppy-ProviderBrowserItem', { 'uppy-ProviderBrowserItem--disabled': isDisabled }, { 'uppy-ProviderBrowserItem--noPreview': item.data.icon === 'video' }, { 'uppy-ProviderBrowserItem--is-checked': item.status === 'checked' }, { 'uppy-ProviderBrowserItem--is-partial': item.status === 'partial' }), title: ('restrictionError' in item ? item.restrictionError : undefined) ??
            undefined, children: [_jsx("input", { type: "checkbox", className: "uppy-u-reset uppy-ProviderBrowserItem-checkbox", onChange: () => toggleCheckbox(item, false), checked: item.status === 'checked', "aria-label": item.data.name ?? i18n('unnamed'), disabled: isDisabled, "data-uppy-super-focusable": true }), _jsxs("button", { type: "button", className: "uppy-u-reset uppy-c-btn uppy-ProviderBrowserItem-inner", disabled: isDisabled, "aria-label": item.data.name, onClick: () => {
                    if (item.data.isFolder) {
                        openFolder(item.id);
                    }
                }, children: [_jsx("div", { className: "uppy-ProviderBrowserItem-iconWrap", children: _jsx(ItemIcon, { itemIconString: item.data.icon }) }), item.data.name ?? i18n('unnamed')] })] }));
};
export default SearchResultItem;
