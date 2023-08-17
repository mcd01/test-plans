package utils

import (
	"fmt"
	"strconv"
	"strings"
)

func ParseIntArray(value string) ([]int, error) {
	var intValues []int
	stringValues := strings.Split(value, ",")
	for _, str := range stringValues {
		num, err := strconv.ParseInt(str, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("could not convert '%s' to integer(s)", stringValues)
		}
		intValues = append(intValues, int(num))
	}
	return intValues, nil
}
