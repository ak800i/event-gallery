/**
 * Hook to create a form element outside the component tree to avoid nested forms.
 * Returns a formId that can be used with the `form` attribute on inputs and buttons.
 *
 * This allows form submission (Enter key) to work properly even when the component
 * is rendered inside another form element.
 *
 * @param onSubmit - Callback to execute when the form is submitted
 * @returns Object containing the formId to use with form attribute
 */
export declare function useSearchForm(onSubmit: () => void): {
    formId: string;
};
//# sourceMappingURL=useSearchForm.d.ts.map