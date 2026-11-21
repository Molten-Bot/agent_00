package workspace

func diskPoolID(path string, _ uint32) (string, error) {
	return diskDeviceID(path)
}
