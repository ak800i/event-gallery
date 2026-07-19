import type { I18n } from '@uppy/utils';
interface FilterInputProps {
    value: string;
    onChange: (value: string) => void;
    onSubmit: () => void;
    inputLabel: string;
    i18n: I18n;
}
/**
 * FilterInput component for client-side filtering with search icon and clear button.
 * Supports Enter key submission via form element.
 */
declare function FilterInput({ value, onChange, onSubmit, inputLabel, i18n, }: FilterInputProps): import("preact").JSX.Element;
export default FilterInput;
//# sourceMappingURL=FilterInput.d.ts.map