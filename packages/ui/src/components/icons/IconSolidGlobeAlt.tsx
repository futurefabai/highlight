import React from 'react'
import { IconProps } from './types'

export const IconSolidGlobeAlt = ({ size = '1em', ...props }: IconProps) => {
	return (
		<svg
			xmlns="http://www.w3.org/2000/svg"
			width={size}
			height={size}
			fill="none"
			viewBox="0 0 20 20"
			focusable="false"
			{...props}
		>
			<path
				fill="currentColor"
				fillRule="evenodd"
				d="M4.083 9h1.946c.089-1.546.383-2.97.837-4.118A6.004 6.004 0 0 0 4.083 9ZM10 2a8 8 0 1 0 0 16 8 8 0 0 0 0-16Zm0 2c-.076 0-.232.032-.465.262-.238.234-.497.623-.737 1.182-.389.907-.673 2.142-.766 3.556h3.936c-.093-1.414-.377-2.649-.766-3.556-.24-.56-.5-.948-.737-1.182C10.232 4.032 10.076 4 10 4Zm3.971 5c-.089-1.546-.383-2.97-.837-4.118A6.004 6.004 0 0 1 15.917 9h-1.946Zm-2.003 2H8.032c.093 1.414.377 2.649.766 3.556.24.56.5.948.737 1.182.233.23.389.262.465.262.076 0 .232-.032.465-.262.238-.234.498-.623.737-1.182.389-.907.673-2.142.766-3.556Zm1.166 4.118c.454-1.147.748-2.572.837-4.118h1.946a6.004 6.004 0 0 1-2.783 4.118Zm-6.268 0C6.412 13.97 6.118 12.546 6.03 11H4.083a6.004 6.004 0 0 0 2.783 4.118Z"
				clipRule="evenodd"
			/>
		</svg>
	)
}
