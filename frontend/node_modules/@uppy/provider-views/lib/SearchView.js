import { jsx as _jsx, jsxs as _jsxs } from "preact/jsx-runtime";
import { useSearchForm } from './useSearchForm.js';
/**
 * SearchView component for search with a submit button.
 * Typically used for initial search views or forms that require explicit submission.
 * The children prop is rendered as the button content, allowing full control over button text and loading states.
 */
function SearchView({ value, onChange, onSubmit, inputLabel, loading = false, children, }) {
    const { formId } = useSearchForm(onSubmit);
    return (_jsxs("section", { className: "uppy-SearchProvider", children: [_jsx("input", { className: "uppy-u-reset uppy-c-textInput uppy-SearchProvider-input", type: "search", "aria-label": inputLabel, placeholder: inputLabel, value: value, onInput: (e) => onChange(e.target.value), form: formId, disabled: loading, "data-uppy-super-focusable": true }), _jsx("button", { disabled: loading, className: "uppy-u-reset uppy-c-btn uppy-c-btn-primary uppy-SearchProvider-searchButton", type: "submit", form: formId, children: children })] }));
}
export default SearchView;
